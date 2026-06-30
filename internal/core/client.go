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
	mu             sync.RWMutex // protects session/config/tunConfig swaps
	config         *Config
	tunConfig      *OPENACKResult // baseline TUN config from initial handshake
	TUN            TunDevice
	session        *Session
	stopCh         chan struct{}
	stopped        bool
	closeOnce      sync.Once
	packetPumpOnce sync.Once  // ensures tunToServer goroutine is launched once
	switchMu       sync.Mutex // serializes SwitchServer calls
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

// SetTunnelConfig stores the baseline TUN configuration from the initial
// handshake so SwitchServer can validate compatibility with new servers.
func (c *Client) SetTunnelConfig(t *OPENACKResult) {
	c.mu.Lock()
	c.tunConfig = t
	c.mu.Unlock()
}

// currentTunConfig returns a snapshot of the baseline tunnel config.
func (c *Client) currentTunConfig() *OPENACKResult {
	c.mu.RLock()
	t := c.tunConfig
	c.mu.RUnlock()
	return t
}

// currentEncrypt returns the current encrypt setting under the read lock.
func (c *Client) currentEncrypt() int {
	c.mu.RLock()
	e := c.config.Encrypt
	c.mu.RUnlock()
	return e
}

// cloneConfig returns a shallow copy of cfg so SwitchServer can use a
// modified config without mutating the caller's pointer.
func cloneConfig(cfg *Config) *Config {
	if cfg == nil {
		return nil
	}
	cpy := *cfg
	return &cpy
}

// checkTunnelCompatible returns an error if the new OPENACKResult requires an
// unsupported reconfiguration. Server-assigned LocalIP/GatewayIP changes are
// allowed and applied in-place by applyTunnelConfig; MTU changes are still
// rejected for now because MTU reconfiguration is separate.
func (c *Client) checkTunnelCompatible(newCfg *OPENACKResult) error {
	old := c.currentTunConfig()
	if old == nil {
		return fmt.Errorf("no baseline tunnel config: call SetTunnelConfig first")
	}
	if newCfg.MTU > 0 {
		cfg := c.currentConfig()
		if int(newCfg.MTU) != cfg.MTU {
			return fmt.Errorf("MTU mismatch: new=%d current=%d", newCfg.MTU, cfg.MTU)
		}
	}
	return nil
}

// applyTunnelConfig applies server-assigned tunnel IP changes to the existing
// TUN adapter before publishing a new session. It never closes/recreates TUN
// and does not touch routes; existing routes are interface-based.
func (c *Client) applyTunnelConfig(tunCfg *OPENACKResult, cfg *Config) error {
	if tunCfg == nil {
		return fmt.Errorf("nil tunnel config")
	}
	old := c.currentTunConfig()
	if old == nil {
		return fmt.Errorf("no baseline tunnel config: call SetTunnelConfig first")
	}
	if tunCfg.LocalIP == old.LocalIP && tunCfg.GatewayIP == old.GatewayIP {
		return nil
	}
	if c.TUN == nil {
		return fmt.Errorf("cannot reconfigure tunnel IP: TUN is nil")
	}

	localCIDR := tunCfg.LocalIP + "/24"
	log.Printf("[SWITCH] Reconfiguring TUN %s IP %s/%s -> %s/%s",
		c.TUN.Name(), old.LocalIP, old.GatewayIP, tunCfg.LocalIP, tunCfg.GatewayIP)
	if err := SetTUNIP(c.TUN.Name(), localCIDR, tunCfg.GatewayIP); err != nil {
		return fmt.Errorf("set switched TUN IP: %w", err)
	}

	c.mu.Lock()
	c.tunConfig = tunCfg
	c.mu.Unlock()
	_ = cfg // kept for future MTU/address reconfiguration without changing signature
	return nil
}

// isCurrentSession reports whether s is the currently active session.
func (c *Client) isCurrentSession(s *Session) bool {
	c.mu.RLock()
	cur := c.session
	c.mu.RUnlock()
	return s == cur
}

// currentConfig returns a snapshot of the active config pointer.
func (c *Client) currentConfig() *Config {
	c.mu.RLock()
	cfg := c.config
	c.mu.RUnlock()
	return cfg
}

// currentBindHint returns a safe source IP hint for the next UDP dial during
// server switch. It reuses the current session's source IP only when it is a
// stable non-tunnel address (never 10.100.100.* and never the current TUN IP).
func (c *Client) currentBindHint() *net.UDPAddr {
	s := c.currentSession()
	if s == nil || s.conn == nil {
		return nil
	}
	cur, ok := s.conn.LocalAddr().(*net.UDPAddr)
	if !ok || cur == nil || cur.IP == nil {
		return nil
	}
	ip := cur.IP.To4()
	if ip == nil {
		return nil
	}
	if ip[0] == 10 && ip[1] == 100 && ip[2] == 100 {
		return nil
	}
	old := c.currentTunConfig()
	if old != nil {
		if oldIP := net.ParseIP(old.LocalIP); oldIP != nil && oldIP.Equal(cur.IP) {
			return nil
		}
	}
	return &net.UDPAddr{IP: append(net.IP(nil), cur.IP...), Port: 0}
}

func (c *Client) validateSwitchSourceBind(s *Session, tunCfg *OPENACKResult) error {
	if s == nil || s.conn == nil {
		return nil
	}
	addr, ok := s.conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr == nil || addr.IP == nil {
		return nil
	}
	ip := addr.IP.To4()
	if ip == nil || !(ip[0] == 10 && ip[1] == 100 && ip[2] == 100) {
		return nil
	}
	newIP := net.ParseIP(tunCfg.LocalIP)
	old := c.currentTunConfig()
	var oldIP net.IP
	if old != nil {
		oldIP = net.ParseIP(old.LocalIP)
	}
	if newIP == nil || !addr.IP.Equal(newIP) || (oldIP != nil && !oldIP.Equal(newIP) && addr.IP.Equal(oldIP)) {
		return fmt.Errorf("switch: stale source bind %s for tunnel ip %s", addr.IP.String(), tunCfg.LocalIP)
	}
	return nil
}

// StatusResult is a read-only snapshot of the current tunnel state for the
// control API. All fields are thread-safe snapshots.
type StatusResult struct {
	State     string `json:"state"` // "running" or "disconnected"
	Server    string `json:"server"`
	Port      int    `json:"port"`
	SessionID uint16 `json:"session_id"`
	TUN       string `json:"tun"`
	LocalIP   string `json:"local_ip"`
	GatewayIP string `json:"gateway_ip"`
	Route     string `json:"route"`
	MTU       int    `json:"mtu"`
}

// Status returns a thread-safe snapshot of the current tunnel state.
func (c *Client) Status() *StatusResult {
	c.mu.RLock()
	defer c.mu.RUnlock()

	sr := &StatusResult{State: "disconnected"}

	if c.session != nil && c.session.id != 0 {
		sr.State = "running"
		sr.SessionID = c.session.id
	}

	if c.config != nil {
		sr.Server = c.config.Server
		sr.Port = c.config.Port
		sr.Route = c.config.RouteNet
		sr.MTU = c.config.MTU
	}

	if c.tunConfig != nil {
		sr.LocalIP = c.tunConfig.LocalIP
		sr.GatewayIP = c.tunConfig.GatewayIP
	}

	if c.TUN != nil {
		sr.TUN = c.TUN.Name()
	}

	return sr
}

// newSession resolves and dials the UDP server from config, returning a
// Session with the live connection but without performing a handshake.
func newSession(cfg *Config, localAddr *net.UDPAddr) (*Session, error) {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", cfg.Server, cfg.Port))
	if err != nil {
		return nil, fmt.Errorf("resolve server: %w", err)
	}

	conn, err := net.DialUDP("udp", localAddr, addr)
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
	s, err := newSession(c.config, nil)
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

// Start launches the per-session loops (heartbeat + server→TUN) and the
// adapter-lifetime TUN→server packet pump, then returns immediately.
// Unlike Run() which blocks, Start() is designed for daemon-style callers
// that keep the Client alive across multiple SwitchServer calls.
// Must be called after Handshake and after TUN has been configured.
func (c *Client) Start() error {
	s := c.currentSession()
	if s == nil {
		return fmt.Errorf("not connected: call Connect first")
	}

	log.Println("[INFO] Tunnel established, starting daemon loops...")
	c.startSessionLoops(s)

	// Preserve Run()'s protocol timing: the server expects the first ECHOREQ
	// before accepting DATA, so delay TUN forwarding briefly.
	time.Sleep(3 * time.Second)
	c.startPacketPumpOnce()
	return nil
}

// startPacketPumpOnce launches the adapter-lifetime TUN→server goroutine
// exactly once. Safe to call from Run, Start, and SwitchServer paths.
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

// runSessionToTUN calls sessionToTUN(s) and logs the outcome differently
// depending on whether the session is still current when it exits. This gives
// clean log output during a SwitchServer transition (the old session's exit is
// expected, not an error).
func (c *Client) runSessionToTUN(s *Session) {
	if err := c.sessionToTUN(s); err != nil {
		if c.isCurrentSession(s) {
			log.Printf("[ERROR] Active session ended: %v", err)
			c.failSession(s, err)
		} else {
			log.Printf("[INFO] Previous session ended: %v", err)
		}
	}
}

// startSessionLoops launches the per-session heartbeat and server→TUN
// goroutines for the given session. Both goroutines are bounded to the
// session's lifetime (done channel) and the Client stopCh.
func (c *Client) startSessionLoops(s *Session) {
	go c.heartbeatLoop(s)
	go c.runSessionToTUN(s)
}

// failSession atomically clears c.session if it still points to s, then
// closes s. This is the safe reaction to a session-level write failure
// (e.g. wsasend or connection refused after a network change).
//
// It does NOT close TUN, the Client, or the daemon — a future switch or
// reconnect can create a fresh session on the same TUN adapter.
func (c *Client) failSession(s *Session, reason error) {
	if s == nil {
		return
	}
	c.mu.Lock()
	if c.session == s {
		c.session = nil
	}
	c.mu.Unlock()
	log.Printf("[SESSION] Failing session %d: %v", s.id, reason)
	s.Close()
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
		pkt := buildDataPacket(s.id, s.seq, buf[:n], c.currentEncrypt())
		if _, err := s.conn.Write(pkt); err != nil {
			c.failSession(s, fmt.Errorf("tun write: %w", err))
		}
	}
}

// heartbeatLoop sends ECHOREQ every 2 seconds; first one fires immediately.
// Returns when either the Client stopCh or the Session done channel closes,
// so a per-session teardown cancels the heartbeat without waiting for a full
// Client shutdown.
// Write errors trigger a session failure and exit the loop so the dead
// UDP socket is torn down (important after network changes).
func (c *Client) heartbeatLoop(s *Session) {
	sendBeat := func(s *Session) {
		s.echoCnt++
		ts := uint64(time.Now().UnixNano() / 1000)
		pkt := BuildEchoReq(s.id, s.seq, ts, s.pipeID, s.pipeIdx, s.echoCnt)
		if _, err := s.conn.Write(pkt); err != nil {
			log.Printf("[ERROR] Send ECHOREQ: %v", err)
			c.failSession(s, err)
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
func connectAndHandshakeSession(cfg *Config, localAddr *net.UDPAddr) (*Session, []byte, error) {
	s, err := newSession(cfg, localAddr)
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

// SwitchServer connects and handshakes to the server described by next,
// validates that the new server is tunnel-compatible with the existing TUN
// configuration, then atomically swaps the active session and config.
//
// On success the old session is torn down, heartbeat and server→TUN
// goroutines are started for the new session, and the parsed OPENACK is
// returned. On failure the new session is closed and an error is returned;
// the existing session is left untouched.
func (c *Client) SwitchServer(next *Config) (*OPENACKResult, error) {
	if !c.switchMu.TryLock() {
		return nil, fmt.Errorf("switch already in progress")
	}
	defer c.switchMu.Unlock()

	// a) clone + validate
	nextCfg := cloneConfig(next)
	if nextCfg == nil {
		return nil, fmt.Errorf("switch: nil config")
	}
	if err := nextCfg.Validate(); err != nil {
		return nil, fmt.Errorf("switch: invalid config: %w", err)
	}

	// b) connect + handshake
	bindHint := c.currentBindHint()
	if bindHint != nil {
		log.Printf("[SWITCH] Binding new session to source=%s", bindHint.IP.String())
	}
	newS, raw, err := connectAndHandshakeSession(nextCfg, bindHint)
	if err != nil {
		return nil, fmt.Errorf("switch: %w", err)
	}

	// c) parse OPENACK
	tunCfg := ParseOPENACK(raw)
	if tunCfg.LocalIP == "" || tunCfg.GatewayIP == "" {
		newS.Close()
		return nil, fmt.Errorf("switch: OPENACK missing IP info")
	}
	if err := c.validateSwitchSourceBind(newS, tunCfg); err != nil {
		newS.Close()
		return nil, err
	}

	// d) check tunnel compatibility
	if err := c.checkTunnelCompatible(tunCfg); err != nil {
		newS.Close()
		return nil, fmt.Errorf("switch: incompatible: %w", err)
	}
	if err := c.applyTunnelConfig(tunCfg, nextCfg); err != nil {
		newS.Close()
		return nil, fmt.Errorf("switch: tunnel reconfig: %w", err)
	}

	// Override config MTU before publishing nextCfg so readers never observe
	// the pre-OPENACK effective MTU after the switch is committed.
	if tunCfg.MTU > 0 {
		nextCfg.MTU = int(tunCfg.MTU)
	}

	// e+f) atomically swap session + config
	c.mu.Lock()
	old := c.session
	c.session = newS
	c.config = nextCfg
	c.mu.Unlock()

	// g) close old session after swap (so tunToServer sees new session)
	if old != nil {
		old.Close()
	}

	// h) launch per-session goroutines for the new session
	c.startSessionLoops(newS)

	// i) ensure TUN→server pump is running
	c.startPacketPumpOnce()

	log.Printf("[SWITCH] Switched to %s:%d session=%d", nextCfg.Server, nextCfg.Port, newS.id)
	return tunCfg, nil
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
