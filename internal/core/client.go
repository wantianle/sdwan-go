package core

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

// TunDevice abstracts a TUN virtual network device.
// It provides simple Read/Write for IP packets, plus name query and close.
type TunDevice interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Name() string
	Close() error
}

// Session holds the UDP connection and protocol session state for one server.
// Extracted from Client so future daemon/server-switch paths can create,
// teardown, and swap sessions independently of the TUN/adapter lifecycle.
type Session struct {
	conn    *net.UDPConn
	server  *net.UDPAddr
	id      uint16
	seq     uint32
	echoCnt uint32
	pipeID  uint32
	pipeIdx uint32
}

// Client is the SDWAN tunnel client
type Client struct {
	config    *Config
	TUN       TunDevice
	session   *Session
	stopCh    chan struct{}
	stopped   bool
	closeOnce sync.Once
}

// NewClient creates a new SDWAN client
func NewClient(cfg *Config) (*Client, error) {
	return &Client{
		config: cfg,
		stopCh: make(chan struct{}),
	}, nil
}

// Connect opens the UDP socket and initialises the Session.
func (c *Client) Connect() error {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", c.config.Server, c.config.Port))
	if err != nil {
		return fmt.Errorf("resolve server: %w", err)
	}

	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return fmt.Errorf("dial UDP: %w", err)
	}

	c.session = &Session{
		conn:    conn,
		server:  addr,
		pipeID:  uint32(c.config.PipeID),
		pipeIdx: uint32(c.config.PipeIdx),
	}
	return nil
}

// SessionID returns the current protocol session identifier.
// Returns 0 if the client has not completed a handshake.
func (c *Client) SessionID() uint16 {
	if c.session == nil {
		return 0
	}
	return c.session.id
}

// Handshake sends OPEN and waits for OPENACK. Returns the raw OPENACK data.
// Must be called after Connect.
func (c *Client) Handshake() ([]byte, error) {
	s := c.session
	if s == nil {
		return nil, fmt.Errorf("not connected: call Connect first")
	}

	// Send OPEN
	openPkt := BuildOpenPacket(c.config)
	log.Println("[AUTH] Sending OPEN...")
	if _, err := s.conn.Write(openPkt); err != nil {
		return nil, fmt.Errorf("send OPEN: %w", err)
	}

	// Wait for OPENACK
	buf := make([]byte, 2048)
	s.conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	for {
		n, err := s.conn.Read(buf)
		if err != nil {
			return nil, fmt.Errorf("read OPENACK: %w", err)
		}
		data := buf[:n]
		if len(data) < 24 {
			continue
		}
		mt := MsgType(data)
		if mt == MsgOPENACK {
			if !PktVerify(data) {
				log.Println("[AUTH] OPENACK signature mismatch, retrying...")
				s.conn.Write(openPkt)
				continue
			}
			s.id = ParseSessionID(data)
			s.seq = ParseOPENACKSeq(data)
			s.conn.SetReadDeadline(time.Time{})
			log.Printf("[AUTH] OPENACK received, session=%d seq=%d", s.id, s.seq)
			return data, nil
		}
		if mt == 0x11 || mt == 0xff {
			return nil, fmt.Errorf("peer AUTH REJECTED")
		}
	}
}

// Run starts the main event loop (heartbeat + data forwarding).
// Must be called after Handshake.
func (c *Client) Run() error {
	s := c.session
	if s == nil {
		return fmt.Errorf("not connected: call Connect first")
	}

	log.Println("[INFO] Tunnel established, starting main loop...")

	// Heartbeat goroutine — fires first beat immediately
	go c.heartbeatLoop(s)

	// Delay TUN forwarding until session is stable.
	// The server requires the first ECHOREQ handshake before accepting DATA.
	time.Sleep(3 * time.Second)

	// Read from TUN → send to server
	go c.tunToServer(s)

	// Main loop: read from server → write to TUN
	buf := make([]byte, 2048)
	for {
		n, err := s.conn.Read(buf)
		if err != nil {
			log.Printf("[ERROR] Read from server: %v", err)
			return err
		}
		data := buf[:n]
		mt := MsgType(data)

		switch mt {
		case MsgECHORESP:
			// heartbeat response, consume silently
		case MsgTUNSetup, MsgDATA:
			// 0x14 = unencrypted DATA, 0x18 = encrypted DATA
			// Both share 8-byte header, skip it for TUN write
			if len(data) > 8 {
				if c.TUN != nil {
					c.TUN.Write(data[8:])
				}
			}
		case 0x11: // CLOSE
			log.Println("[WARN] Server sent CLOSE, reconnecting...")
			return fmt.Errorf("server CLOSE")
		}
	}
}

// tunToServer reads from TUN device and sends to server
func (c *Client) tunToServer(s *Session) {
	buf := make([]byte, 2048)
	for {
		select {
		case <-c.stopCh:
			return
		default:
		}
		if c.TUN == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		n, err := c.TUN.Read(buf)
		if err != nil {
			if c.stopped {
				return
			}
			time.Sleep(50 * time.Millisecond) // prevent tight spin on transient error
			continue
		}
		if s == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		pkt := buildDataPacket(s.id, s.seq, buf[:n], c.config.Encrypt)
		s.conn.Write(pkt)
	}
}

// heartbeatLoop sends ECHOREQ every 2 seconds; first one fires immediately.
// Respects stopCh so the goroutine exits cleanly on Close().
func (c *Client) heartbeatLoop(s *Session) {
	sendBeat := func(s *Session) {
		s.echoCnt++
		ts := uint64(time.Now().UnixNano() / 1000)
		pkt := BuildEchoReq(s.id, s.seq, ts, s.pipeID, s.pipeIdx, s.echoCnt)
		if _, err := s.conn.Write(pkt); err != nil {
			log.Printf("[ERROR] Send ECHOREQ: %v", err)
		}
	}

	if s == nil {
		return
	}

	// Fire first heartbeat immediately
	sendBeat(s)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			sendBeat(s)
		}
	}
}

// buildDataPacket constructs a DATA packet.
// encrypt=0 → type 0x14 (plain TUN data)
// encrypt=1 → type 0x18 (AES-encrypted TUN data)
// Confirmed by reverse engineering sdwclnt_tun_recv @ 0x40576b.
func buildDataPacket(sessionID uint16, seq uint32, payload []byte, encrypt int) []byte {
	hdr := make([]byte, 8)
	if encrypt != 0 {
		hdr[0] = MsgDATA // 0x18
	} else {
		hdr[0] = MsgTUNSetup // 0x14
	}
	hdr[1] = byte(encrypt)
	binary.BigEndian.PutUint16(hdr[2:4], sessionID)
	binary.BigEndian.PutUint32(hdr[4:8], seq)

	pkt := make([]byte, 8+len(payload))
	copy(pkt[:8], hdr)
	copy(pkt[8:], payload)
	return pkt
}

// Close cleans up resources. Safe to call multiple times.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		c.stopped = true
		close(c.stopCh)
		if c.session != nil && c.session.conn != nil {
			c.session.conn.Close()
		}
	})
}
