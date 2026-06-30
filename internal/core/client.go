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
	conn      *net.UDPConn
	server    *net.UDPAddr
	id        uint16
	seq       uint32
	echoCnt   uint32
	pipeID    uint32
	pipeIdx   uint32
	done      chan struct{} // closed when session is torn down
	closeOnce sync.Once
}

// Client is the SDWAN tunnel client
type Client struct {
	mu             sync.RWMutex // protects session pointer swaps
	config         *Config
	TUN            TunDevice
	session        *Session
	stopCh         chan struct{}
	stopped        bool
	closeOnce      sync.Once
	packetPumpOnce sync.Once // ensures tunToServer goroutine is launched once
}

// NewClient creates a new SDWAN client
func NewClient(cfg *Config) (*Client, error) {
	return &Client{
		config: cfg,
		stopCh: make(chan struct{}),
	}, nil
}

// currentSession returns the active session pointer under the read lock.
// The returned pointer is a snapshot — callers must not rely on it
// remaining current after the lock is released.
func (c *Client) currentSession() *Session {
	c.mu.RLock()
	s := c.session
	c.mu.RUnlock()
	return s
}

// setSession atomically swaps the active session pointer and returns the
// previous session (nil if none). The old session is NOT closed by this
// helper — callers are responsible for closing it if needed.
func (c *Client) setSession(s *Session) (old *Session) {
	c.mu.Lock()
	old = c.session
	c.session = s
	c.mu.Unlock()
	return
}

// newSession resolves and dials the UDP server from config, returning a
// Session with the live connection but without performing a handshake.
func newSession(cfg *Config) (*Session, error) {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", cfg.Server, cfg.Port))
	if err != nil {
		return nil, fmt.Errorf("resolve server: %w", err)
	}

	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return nil, fmt.Errorf("dial UDP: %w", err)
	}

	return &Session{
		conn:    conn,
		server:  addr,
		pipeID:  uint32(cfg.PipeID),
		pipeIdx: uint32(cfg.PipeIdx),
		done:    make(chan struct{}),
	}, nil
}

// Connect opens the UDP socket and initialises the Session.
// If a session already exists it is closed before the new one is assigned.
func (c *Client) Connect() error {
	s, err := newSession(c.config)
	if err != nil {
		return err
	}
	old := c.setSession(s)
	if old != nil {
		old.Close()
	}
	return nil
}

// SessionID returns the current protocol session identifier.
// Returns 0 if the client has not completed a handshake.
func (c *Client) SessionID() uint16 {
	s := c.currentSession()
	if s == nil {
		return 0
	}
	return s.id
}

// Handshake sends OPEN and waits for OPENACK. Returns the raw OPENACK data.
// Must be called after Connect.
func (c *Client) Handshake() ([]byte, error) {
	s := c.currentSession()
	if s == nil {
		return nil, fmt.Errorf("not connected: call Connect first")
	}
	return s.Handshake(c.config)
}

// Handshake sends the OPEN packet over this session's UDP connection and
// blocks until a valid signed OPENACK arrives. On success the session is
// populated with the negotiated session id and sequence number.
//
// Must be called after dial — callers own the Session lifecycle and must
// Close the session on error if the session should not be reused.
func (s *Session) Handshake(cfg *Config) ([]byte, error) {
	if s == nil || s.conn == nil {
		return nil, fmt.Errorf("session not connected")
	}

	// Send OPEN
	openPkt := BuildOpenPacket(cfg)
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
	s := c.currentSession()
	if s == nil {
		return fmt.Errorf("not connected: call Connect first")
	}

	log.Println("[INFO] Tunnel established, starting main loop...")

	// Heartbeat goroutine — fires first beat immediately, operates on
	// the snapshot captured at Run entry.
	go c.heartbeatLoop(s)

	// Delay TUN forwarding until session is stable.
	// The server requires the first ECHOREQ handshake before accepting DATA.
	time.Sleep(3 * time.Second)

	// Start the adapter-lifetime TUN→server packet pump (idempotent
	// across multiple Run calls).
	c.startPacketPumpOnce()

	return c.sessionToTUN(s)
}

// startPacketPumpOnce launches the adapter-lifetime TUN→server goroutine
// exactly once. Safe to call from every Run() invocation.
func (c *Client) startPacketPumpOnce() {
	c.packetPumpOnce.Do(func() {
		go c.tunToServer()
	})
}

// sessionToTUN reads packets from the given session and writes them to TUN.
// Returns when the session connection closes or errors, allowing the caller
// to restart the loop with a new session.
func (c *Client) sessionToTUN(s *Session) error {
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

// tunToServer reads from the TUN device and forwards packets to the active
// session. It calls currentSession() per-packet so a future session swap is
// picked up without restarting the goroutine.
//
// When no active session exists the packet is silently dropped to avoid
// backpressure during a switch transition.
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
		s := c.currentSession()
		if s == nil {
			// drop packet — no active session
			continue
		}
		pkt := buildDataPacket(s.id, s.seq, buf[:n], c.config.Encrypt)
		s.conn.Write(pkt)
	}
}

// heartbeatLoop sends ECHOREQ every 2 seconds; first one fires immediately.
// Returns when either the Client stopCh or the Session done channel closes,
// so a per-session teardown cancels the heartbeat without waiting for a full
// Client shutdown.
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
	select {
	case <-c.stopCh:
		return
	case <-s.Done():
		return
	default:
	}

	// Fire first heartbeat immediately
	sendBeat(s)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-s.Done():
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

// connectAndHandshakeSession dials and performs the full SD-WAN handshake
// in one call. On handshake failure the session is closed to avoid leaking
// the UDP socket. Callers that need the raw OPENACK payload (e.g. for TUN
// config) receive it as the second return value.
//
// This is purely a convenience helper — the existing one-shot path in
// RunOnce continues to use Client.Connect + Client.Handshake separately.
func connectAndHandshakeSession(cfg *Config) (*Session, []byte, error) {
	s, err := newSession(cfg)
	if err != nil {
		return nil, nil, err
	}
	raw, err := s.Handshake(cfg)
	if err != nil {
		s.Close()
		return nil, nil, err
	}
	return s, raw, nil
}

// Close nil-safely and idempotently closes the underlying UDP connection.
// It signals session cancellation via the done channel before closing conn
// so goroutines watching Done() can exit cleanly.
func (s *Session) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		if s.done != nil {
			close(s.done)
		}
		if s.conn != nil {
			_ = s.conn.Close()
		}
	})
}

// Done returns a channel that is closed when the session is torn down.
// Goroutines can select on this alongside stopCh for per-session cancellation.
func (s *Session) Done() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.done
}

// closeSession atomically swaps the session pointer to nil and closes the
// previous session if one existed.
func (c *Client) closeSession() {
	old := c.setSession(nil)
	if old != nil {
		old.Close()
	}
}

// Close cleans up resources. Safe to call multiple times.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		c.stopped = true
		close(c.stopCh)
		c.closeSession()
	})
}
