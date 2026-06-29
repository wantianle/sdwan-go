package core

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
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

// Client is the SDWAN tunnel client
type Client struct {
	config    *Config
	conn      *net.UDPConn
	server    *net.UDPAddr
	TUN       TunDevice
	SessionID uint16
	seq       uint32
	echoCnt   uint32
	pipeID    uint32
	pipeIdx   uint32
	stopCh    chan struct{}
	stopped   bool
}

// NewClient creates a new SDWAN client
func NewClient(cfg *Config) (*Client, error) {
	return &Client{
		config:  cfg,
		pipeID:  uint32(cfg.PipeID),
		pipeIdx: uint32(cfg.PipeIdx),
		stopCh:  make(chan struct{}),
	}, nil
}

// Connect opens the UDP socket and sends the OPEN request
func (c *Client) Connect() error {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", c.config.Server, c.config.Port))
	if err != nil {
		return fmt.Errorf("resolve server: %w", err)
	}
	c.server = addr

	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return fmt.Errorf("dial UDP: %w", err)
	}
	c.conn = conn

	return nil
}

// Handshake sends OPEN and waits for OPENACK. Returns the raw OPENACK data.
func (c *Client) Handshake() ([]byte, error) {
	// Send OPEN
	openPkt := BuildOpenPacket(c.config)
	log.Println("[AUTH] Sending OPEN...")
	if _, err := c.conn.Write(openPkt); err != nil {
		return nil, fmt.Errorf("send OPEN: %w", err)
	}

	// Wait for OPENACK
	buf := make([]byte, 2048)
	c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	for {
		n, err := c.conn.Read(buf)
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
				c.conn.Write(openPkt)
				continue
			}
		c.SessionID = ParseSessionID(data)
		c.seq = ParseOPENACKSeq(data)
		c.conn.SetReadDeadline(time.Time{})
		log.Printf("[AUTH] OPENACK received, session=%d seq=%d", c.SessionID, c.seq)
			return data, nil
		}
		if mt == 0x11 || mt == 0xff {
			return nil, fmt.Errorf("peer AUTH REJECTED")
		}
	}
}

// Run starts the main event loop (heartbeat + data forwarding)
func (c *Client) Run() error {
	log.Println("[INFO] Tunnel established, starting main loop...")

	// Heartbeat goroutine — fires first beat immediately
	go c.heartbeatLoop()

	// Delay TUN forwarding until session is stable.
	// The server requires the first ECHOREQ handshake before accepting DATA.
	time.Sleep(3 * time.Second)

	// Read from TUN → send to server
	go c.tunToServer()

	// Main loop: read from server → write to TUN
	buf := make([]byte, 2048)
	for {
		n, err := c.conn.Read(buf)
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
func (c *Client) tunToServer() {
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
		pkt := buildDataPacket(c.SessionID, c.seq, buf[:n], c.config.Encrypt)
		c.conn.Write(pkt)
	}
}

// heartbeatLoop sends ECHOREQ every 2 seconds; first one fires immediately.
func (c *Client) heartbeatLoop() {
	sendBeat := func() {
		c.echoCnt++
		ts := uint64(time.Now().UnixNano() / 1000)
		pkt := BuildEchoReq(c.SessionID, c.seq, ts, c.pipeID, c.pipeIdx, c.echoCnt)
		if _, err := c.conn.Write(pkt); err != nil {
			log.Printf("[ERROR] Send ECHOREQ: %v", err)
		}
	}

	// Fire first heartbeat immediately
	sendBeat()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		sendBeat()
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

// Close cleans up resources
func (c *Client) Close() {
	c.stopped = true
	close(c.stopCh)
	if c.conn != nil {
		c.conn.Close()
	}
}
