package core

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	controlapi "sdwan-go/pkg/controlapi"
	protocol "sdwan-go/pkg/protocol"
)

var errDaemonStopping = errors.New("daemon is stopping")

// serverResolveTimeout bounds a single DNS resolution on the connect/reconnect
// path. The OS resolver can otherwise stall for ~10s (e.g. systemd-resolved at
// 127.0.0.53), which dominates reconnect time; a short bound degrades fast.
const serverResolveTimeout = 3 * time.Second

// resolveLookup is a test seam over DNS resolution; production uses the default
// resolver. It must honor ctx so serverResolveTimeout actually bounds it.
var resolveLookup = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// serverIPTTL bounds how long a cached hostname -> server-IP mapping is reused.
// After this, resolveServerAddr re-resolves so a server IP that changed (the
// DNS record's TTL is only ~160s) is picked up automatically, without needing a
// failed connect to evict it.
const serverIPTTL = 5 * time.Minute

// resolveNow is a test seam for the clock used by the resolved-IP cache. Go's
// time.Now carries a monotonic reading, so time.Since stays immune to wall-clock
// jumps.
var resolveNow = time.Now

// serverIPCache memoizes hostname -> last resolved server IP and when it was
// resolved. When the network just came back, systemd-resolved's upstream is
// often unreachable and DNS stalls or returns "server misbehaving"; a fresh hit
// lets reconnect skip DNS entirely and dial a known-good IP. Entries expire
// after serverIPTTL so a changed server IP is re-resolved automatically. IP
// literals never enter the cache. A failed handshake deliberately does NOT
// evict: during an outage it would only force a DNS re-resolve that stalls —
// the very stall this cache exists to avoid.
var serverIPCache sync.Map // hostname string -> serverIPEntry

// serverIPEntry is a cached resolution: the chosen IP and when it was resolved.
type serverIPEntry struct {
	ip net.IP
	at time.Time
}

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
	mu                    sync.RWMutex // protects session/config/tunConfig/TUN swaps
	config                *Config
	tunConfig             *protocol.OPENACKResult // baseline TUN config from initial handshake
	lastBindHint          *net.UDPAddr
	TUN                   TunDevice
	session               *Session
	stopCh                chan struct{}
	reconnectCh           chan struct{}
	stopped               bool
	closeOnce             sync.Once
	lifecycleMu           sync.Mutex // closes admission before joining lifecycle work
	lifecycleWG           sync.WaitGroup
	packetPumpOnce        sync.Once // ensures tunToServer goroutine is launched once
	packetPumpStarted     atomic.Bool
	reconnectStarted      atomic.Bool
	reconnecting          atomic.Bool
	paused                atomic.Bool
	startupPending        atomic.Bool // true while initial tunnel connect hasn't succeeded
	tunnelReady           atomic.Bool // true after TUN setup + Start() succeed
	switchMu              sync.Mutex  // serializes SwitchServer calls
	routeConflicts        []RouteConflict
	tunCleanupMu          sync.Mutex
	tunCleanupFn          func()        // set by tryStartup on success, called by Close
	beforeFinalReady      func()        // test seam for shutdown/publication interleavings
	beforePausePublish    func()        // test seam for shutdown/pause interleavings
	beforeInitialLaunch   func()        // test seam for initial session-loop interleavings
	beforePumpLaunch      func()        // test seam for packet-pump/reconnect interleavings
	onSessionLoopsStarted func()        // test seam for initial loop publication
	startDelay            time.Duration // protocol delay; tests may set zero
}

// NewClient creates a new SDWAN client
func NewClient(cfg *Config) *Client {
	return &Client{
		config:      cloneConfig(cfg),
		stopCh:      make(chan struct{}),
		reconnectCh: make(chan struct{}, 1),
		startDelay:  3 * time.Second,
	}
}

// beginLifecycle admits one daemon lifecycle operation. Close closes this
// gate before waiting, so no operation can be added while shutdown is joining.
func (c *Client) beginLifecycle() bool {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	c.mu.RLock()
	stopped := c.stopped
	c.mu.RUnlock()
	if stopped {
		return false
	}
	c.lifecycleWG.Add(1)
	return true
}

func (c *Client) endLifecycle() { c.lifecycleWG.Done() }

func (c *Client) acceptingLifecycle() bool {
	c.mu.RLock()
	accepting := !c.stopped
	c.mu.RUnlock()
	return accepting
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

func (c *Client) isStopped() bool {
	c.mu.RLock()
	stopped := c.stopped
	c.mu.RUnlock()
	return stopped
}

// publishReady atomically publishes final lifecycle state only while the
// daemon remains admitted. It is the final state publication for startup and
// switch operations.
func (c *Client) publishReady() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return false
	}
	c.tunnelReady.Store(true)
	return true
}

func (c *Client) publishReadyAndConflicts(conflicts []RouteConflict) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return false
	}
	c.routeConflicts = conflicts
	c.tunnelReady.Store(true)
	return true
}

func (c *Client) publishStartupReady() bool {
	if c.beforeFinalReady != nil {
		c.beforeFinalReady()
	}
	return c.publishReady()
}

func (c *Client) clearReady() {
	c.tunnelReady.Store(false)
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
func (c *Client) SetTunnelConfig(t *protocol.OPENACKResult) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return false
	}
	c.tunConfig = cloneTunnelConfig(t)
	return true
}

// currentTunConfig returns a snapshot of the baseline tunnel config.
func (c *Client) currentTunConfig() *protocol.OPENACKResult {
	c.mu.RLock()
	t := cloneTunnelConfig(c.tunConfig)
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

func cloneTunnelConfig(cfg *protocol.OPENACKResult) *protocol.OPENACKResult {
	if cfg == nil {
		return nil
	}
	cpy := *cfg
	cpy.GateMAC = append([]byte(nil), cfg.GateMAC...)
	return &cpy
}

func (c *Client) currentTUN() TunDevice {
	c.mu.RLock()
	tun := c.TUN
	c.mu.RUnlock()
	return tun
}

func (c *Client) publishTUN(tun TunDevice) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return false
	}
	c.TUN = tun
	return true
}

func (c *Client) clearTUN(tun TunDevice) {
	c.mu.Lock()
	if tun == nil || c.TUN == tun {
		c.TUN = nil
	}
	c.mu.Unlock()
}

// checkTunnelCompatible returns an error if the new protocol.OPENACKResult requires an
// unsupported reconfiguration. Server-assigned LocalIP/GatewayIP changes are
// allowed and applied in-place by applyTunnelConfig; MTU changes are still
// rejected for now because MTU reconfiguration is separate.
func (c *Client) checkTunnelCompatible(newCfg *protocol.OPENACKResult) error {
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
func (c *Client) applyTunnelConfig(tunCfg *protocol.OPENACKResult) error {
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
	tun := c.currentTUN()
	if tun == nil {
		return fmt.Errorf("cannot reconfigure tunnel IP: TUN is nil")
	}

	localCIDR := tunCfg.LocalIP + "/24"
	log.Printf("[SWITCH] Reconfiguring TUN %s IP %s/%s -> %s/%s",
		tun.Name(), old.LocalIP, old.GatewayIP, tunCfg.LocalIP, tunCfg.GatewayIP)
	if err := SetTUNIP(tun.Name(), localCIDR, tunCfg.GatewayIP); err != nil {
		return fmt.Errorf("set switched TUN IP: %w", err)
	}

	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return fmt.Errorf("switch rejected: %w", errDaemonStopping)
	}
	c.tunConfig = cloneTunnelConfig(tunCfg)
	c.mu.Unlock()
	return nil
}

// isCurrentSession reports whether s is the currently active session.
func (c *Client) isCurrentSession(s *Session) bool {
	c.mu.RLock()
	cur := c.session
	c.mu.RUnlock()
	return s == cur
}

// currentConfig returns an immutable snapshot of the active configuration.
func (c *Client) currentConfig() *Config {
	c.mu.RLock()
	cfg := cloneConfig(c.config)
	c.mu.RUnlock()
	return cfg
}

func (c *Client) setConfigMTU(mtu int) {
	c.mu.Lock()
	if !c.stopped && c.config != nil {
		cfg := cloneConfig(c.config)
		cfg.MTU = mtu
		c.config = cfg
	}
	c.mu.Unlock()
}

func copyUDPAddr(a *net.UDPAddr) *net.UDPAddr {
	if a == nil {
		return nil
	}
	return &net.UDPAddr{IP: append(net.IP(nil), a.IP...), Port: a.Port, Zone: a.Zone}
}

func (c *Client) storeLastBindHint(a *net.UDPAddr) {
	c.mu.Lock()
	c.lastBindHint = copyUDPAddr(a)
	c.mu.Unlock()
}

// currentBindHint returns a safe source IP hint for the next UDP dial during
// server switch. It reuses the current session's source IP only when it is a
// stable non-tunnel address (never 10.100.100.* and never the current TUN IP).
func (c *Client) currentBindHint() *net.UDPAddr {
	s := c.currentSession()
	if s != nil && s.conn != nil {
		if cur, ok := s.conn.LocalAddr().(*net.UDPAddr); ok {
			if hint := c.validBindHint(cur); hint != nil {
				return hint
			}
		}
	}
	c.mu.RLock()
	saved := copyUDPAddr(c.lastBindHint)
	c.mu.RUnlock()
	if hint := c.validBindHint(saved); hint != nil {
		log.Printf("[SWITCH] Using saved source bind hint %s", hint.IP.String())
		return hint
	}
	return nil
}

func (c *Client) validBindHint(addr *net.UDPAddr) *net.UDPAddr {
	if addr == nil || addr.IP == nil {
		return nil
	}
	ip := addr.IP.To4()
	if ip == nil || (ip[0] == 10 && ip[1] == 100 && ip[2] == 100) {
		return nil
	}
	old := c.currentTunConfig()
	if old != nil {
		if oldIP := net.ParseIP(old.LocalIP); oldIP != nil && oldIP.Equal(addr.IP) {
			return nil
		}
	}
	if !isLocalPhysicalIP(addr.IP) {
		return nil
	}
	return &net.UDPAddr{IP: append(net.IP(nil), addr.IP...), Port: 0}
}

func isLocalPhysicalIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	want := ip.To4()
	if want == nil {
		want = ip
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return true
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var got net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				got = v.IP
			case *net.IPAddr:
				got = v.IP
			}
			if got != nil && got.Equal(want) {
				return true
			}
		}
	}
	return false
}

func (c *Client) validateSwitchSourceBind(s *Session, tunCfg *protocol.OPENACKResult) error {
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

// Status returns a thread-safe snapshot of the current tunnel state.
func (c *Client) Status() *controlapi.StatusResult {
	c.mu.RLock()
	defer c.mu.RUnlock()

	sr := &controlapi.StatusResult{State: "disconnected"}

	if c.session != nil && c.session.id != 0 && c.tunnelReady.Load() {
		sr.State = "running"
		sr.SessionID = c.session.id
	} else if c.startupPending.Load() || c.reconnecting.Load() {
		sr.State = "reconnecting"
	} else if c.paused.Load() {
		sr.State = "paused"
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

	// Populate route conflicts
	if len(c.routeConflicts) > 0 {
		sr.RouteConflicts = make([]string, len(c.routeConflicts))
		for i, rc := range c.routeConflicts {
			sr.RouteConflicts[i] = fmt.Sprintf("%s:%s", rc.Interface, rc.LocalCIDR)
		}
	}

	return sr
}

// resolveServerAddr turns the configured server into a *net.UDPAddr. An IP
// literal skips DNS entirely. A hostname first consults serverIPCache so a
// known-working IP is reused without touching DNS (important right after a
// network flap); a hit older than serverIPTTL falls through so a changed server
// IP is picked up. On a miss it resolves with a short bounded timeout
// (serverResolveTimeout), preferring IPv4, and caches the chosen IP plus the
// resolution time. The returned error is wrapped by newSession as
// "resolve server: %w".
func resolveServerAddr(server string, port int) (*net.UDPAddr, error) {
	if ip := net.ParseIP(server); ip != nil {
		return &net.UDPAddr{IP: ip, Port: port}, nil
	}

	if cached, ok := serverIPCache.Load(server); ok {
		if entry, ok := cached.(serverIPEntry); ok && entry.ip != nil &&
			resolveNow().Sub(entry.at) < serverIPTTL {
			return &net.UDPAddr{IP: entry.ip, Port: port}, nil
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), serverResolveTimeout)
	defer cancel()

	ips, err := resolveLookup(ctx, server)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no addresses for host %q", server)
	}

	now := resolveNow()
	// Prefer IPv4.
	for _, ia := range ips {
		if v4 := ia.IP.To4(); v4 != nil {
			serverIPCache.Store(server, serverIPEntry{ip: v4, at: now})
			return &net.UDPAddr{IP: v4, Port: port}, nil
		}
	}
	serverIPCache.Store(server, serverIPEntry{ip: ips[0].IP, at: now})
	return &net.UDPAddr{IP: ips[0].IP, Port: port}, nil
}

// newSession resolves and dials the UDP server from config, returning a
// Session with the live connection but without performing a handshake.
func newSession(cfg *Config, localAddr *net.UDPAddr) (*Session, error) {
	addr, err := resolveServerAddr(cfg.Server, cfg.Port)
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
	cfg := c.currentConfig()
	if cfg == nil || c.isStopped() {
		return fmt.Errorf("connect rejected: daemon is stopping")
	}
	s, err := newSession(cfg, nil)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		s.Close()
		return fmt.Errorf("connect rejected: daemon is stopping")
	}
	old := c.session
	c.session = s
	c.mu.Unlock()
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
	cfg := c.currentConfig()
	if cfg == nil || c.isStopped() {
		return nil, fmt.Errorf("handshake rejected: daemon is stopping")
	}
	return s.Handshake(cfg)
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
	openPkt := protocol.BuildOpenPacket(protocol.OpenConfig{
		Username: cfg.Username,
		Password: cfg.Password,
		MTU:      cfg.MTU,
		Encrypt:  cfg.Encrypt,
	})
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
		mt := protocol.MsgType(data)
		if mt == protocol.MsgOPENACK {
			if !protocol.PktVerify(data) {
				log.Println("[AUTH] OPENACK signature mismatch, retrying...")
				s.conn.Write(openPkt)
				continue
			}
			s.id, _ = protocol.ParseSessionID(data)
			s.seq = protocol.ParseOPENACKSeq(data)
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

	if c.beforeInitialLaunch != nil {
		c.beforeInitialLaunch()
	}
	// Publish session loops immediately, preserving the original first ECHOREQ
	// timing. Close shares this boundary, so shutdown cannot start them late.
	c.lifecycleMu.Lock()
	stopped := c.isStopped()
	if !stopped {
		log.Println("[INFO] Tunnel established, starting daemon loops...")
		c.startSessionLoops(s)
	}
	c.lifecycleMu.Unlock()
	if stopped {
		return errDaemonStopping
	}

	// Preserve Run()'s protocol timing: the server expects the first ECHOREQ
	// before accepting DATA, so delay TUN forwarding briefly.
	select {
	case <-c.stopCh:
		return errDaemonStopping
	case <-time.After(c.startDelay):
	}
	if c.beforePumpLaunch != nil {
		c.beforePumpLaunch()
	}
	// Publish packet forwarding and reconnect only after the protocol delay.
	// c.mu is not held while launching goroutines.
	c.lifecycleMu.Lock()
	stopped = c.isStopped()
	if !stopped {
		c.startPacketPumpOnce()
		c.startReconnect()
	}
	c.lifecycleMu.Unlock()
	if stopped {
		return errDaemonStopping
	}
	return nil
}

func (c *Client) SetPaused(paused bool) bool {
	if c.beforePausePublish != nil {
		c.beforePausePublish()
	}
	// Keep the admission lock through the state transition so Close cannot mark
	// the daemon stopped between accepting a pause request and publishing it.
	c.lifecycleMu.Lock()
	c.mu.Lock()
	stopped := c.stopped
	if !stopped {
		c.paused.Store(paused)
	}
	c.mu.Unlock()
	c.lifecycleMu.Unlock()
	if stopped {
		return false
	}
	if paused {
		c.reconnecting.Store(false)
		if hint := c.currentBindHint(); hint != nil {
			c.storeLastBindHint(hint)
		}
		old := c.setSession(nil)
		if old != nil {
			old.Close()
		}
		return true
	}
	if c.currentSession() == nil && !c.isStopped() {
		select {
		case c.reconnectCh <- struct{}{}:
		default:
		}
	}
	return true
}

func (c *Client) Paused() bool {
	return c.paused.Load()
}

// SetRouteConflicts stores the current route conflict snapshot.
func (c *Client) SetRouteConflicts(conflicts []RouteConflict) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return false
	}
	c.routeConflicts = conflicts
	return true
}

func (c *Client) startReconnect() {
	if !c.reconnectStarted.CompareAndSwap(false, true) {
		return
	}
	go c.reconnectLoop()
}

func (c *Client) reconnectLoop() {
	backoff := 500 * time.Millisecond
	for {
		select {
		case <-c.stopCh:
			c.reconnecting.Store(false)
			return
		case <-c.reconnectCh:
			if c.paused.Load() {
				c.reconnecting.Store(false)
				continue
			}
			c.reconnecting.Store(true)
		}

		for {
			select {
			case <-c.stopCh:
				c.reconnecting.Store(false)
				return
			default:
			}

			if c.currentSession() != nil {
				backoff = 500 * time.Millisecond
				c.reconnecting.Store(false)
				break
			}
			if c.paused.Load() {
				c.reconnecting.Store(false)
				break
			}
			cfg := cloneConfig(c.currentConfig())
			if cfg == nil {
				select {
				case <-c.stopCh:
					c.reconnecting.Store(false)
					return
				case <-time.After(500 * time.Millisecond):
				}
				continue
			} else {
				log.Printf("[RECONNECT] Attempting reconnect to %s:%d", cfg.Server, cfg.Port)
				if _, err := c.SwitchServer(cfg); err != nil {
					if strings.Contains(err.Error(), "switch already in progress") {
						log.Printf("[RECONNECT] Switch already in progress; retrying in %s", backoff)
					} else {
						log.Printf("[RECONNECT] Reconnect failed: %v; retrying in %s", err, backoff)
					}
				} else {
					log.Println("[RECONNECT] Reconnect succeeded")
					backoff = 500 * time.Millisecond
					c.reconnecting.Store(false)
					break
				}
			}

			if c.currentSession() != nil {
				backoff = 500 * time.Millisecond
				c.reconnecting.Store(false)
				break
			}
			if c.paused.Load() {
				c.reconnecting.Store(false)
				break
			}

			select {
			case <-c.stopCh:
				c.reconnecting.Store(false)
				return
			case <-time.After(backoff):
			}
			if backoff < 8*time.Second {
				backoff *= 2
				if backoff > 8*time.Second {
					backoff = 8 * time.Second
				}
			}
		}
	}
}

// startPacketPumpOnce launches the adapter-lifetime TUN→server goroutine
// exactly once. Safe to call from Run, Start, and SwitchServer paths.
func (c *Client) startPacketPumpOnce() {
	c.packetPumpOnce.Do(func() {
		c.packetPumpStarted.Store(true)
		go c.tunToServer()
	})
}

// sessionToTUN reads packets from the given session and writes them to TUN.
// Returns when the session connection closes or errors, allowing the caller
// to restart the loop with a new session.
func (c *Client) sessionToTUN(s *Session) error {
	buf := make([]byte, 2048)
	for {
		// Rolling deadline detects dead UDP sessions after physical network changes;
		// timeout flows through runSessionToTUN -> failSession -> reconnect.
		s.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := s.conn.Read(buf)
		if err != nil {
			log.Printf("[ERROR] Read from server: %v", err)
			return err
		}
		data := buf[:n]
		mt := protocol.MsgType(data)

		switch mt {
		case protocol.MsgECHORESP:
			// heartbeat response, consume silently
		case protocol.MsgTUNSetup, protocol.MsgDATA:
			// 0x14 = unencrypted DATA, 0x18 = encrypted DATA
			// Both share 8-byte header, skip it for TUN write
			if len(data) > 8 {
				if tun := c.currentTUN(); tun != nil {
					tun.Write(data[8:])
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
	if c.onSessionLoopsStarted != nil {
		c.onSessionLoopsStarted()
	}
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
	stopped := c.stopped
	wasCurrent := c.session == s
	if wasCurrent {
		c.session = nil
		c.clearReady()
	}
	c.mu.Unlock()
	log.Printf("[SESSION] Failing session %d: %v", s.id, reason)
	s.Close()
	if wasCurrent && !stopped && !c.paused.Load() {
		select {
		case c.reconnectCh <- struct{}{}:
		default:
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
		tun := c.currentTUN()
		if tun == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		n, err := tun.Read(buf)
		if err != nil {
			if c.isStopped() {
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
		pkt := protocol.BuildEchoReq(s.id, s.seq, ts, s.pipeID, s.pipeIdx, s.echoCnt)
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
		hdr[0] = protocol.MsgDATA // 0x18
	} else {
		hdr[0] = protocol.MsgTUNSetup // 0x14
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
func connectAndHandshakeSession(client *Client, cfg *Config, localAddr *net.UDPAddr) (*Session, []byte, error) {
	s, err := newSession(cfg, localAddr)
	if err != nil {
		return nil, nil, err
	}
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-client.stopCh:
			s.Close()
		case <-finished:
		}
	}()
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
func (c *Client) SwitchServer(next *Config) (*protocol.OPENACKResult, error) {
	if !c.beginLifecycle() {
		return nil, fmt.Errorf("switch rejected: %w", errDaemonStopping)
	}
	defer c.endLifecycle()
	if c.startupPending.Load() {
		return nil, fmt.Errorf("switch rejected: daemon is still starting, wait for running state")
	}
	c.paused.Store(false)

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
	newS, raw, err := connectAndHandshakeSession(c, nextCfg, bindHint)
	if err != nil && bindHint != nil {
		if c.isStopped() {
			return nil, fmt.Errorf("switch interrupted: %w", errDaemonStopping)
		}
		log.Printf("[SWITCH] Bind-hinted session failed (%v); retrying without source bind", err)
		newS, raw, err = connectAndHandshakeSession(c, nextCfg, nil)
	}
	if err != nil {
		if c.isStopped() {
			return nil, fmt.Errorf("switch interrupted: %w", errDaemonStopping)
		}
		return nil, fmt.Errorf("switch: %w", err)
	}
	if !c.acceptingLifecycle() {
		newS.Close()
		return nil, fmt.Errorf("switch rejected: %w", errDaemonStopping)
	}

	// c) parse OPENACK
	tunCfg := protocol.ParseOPENACK(raw)
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
	if err := c.applyTunnelConfig(tunCfg); err != nil {
		newS.Close()
		return nil, fmt.Errorf("switch: tunnel reconfig: %w", err)
	}

	if tunCfg.MTU > 0 {
		nextCfg.MTU = int(tunCfg.MTU)
	}

	// e+f) atomically swap session + config
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		newS.Close()
		return nil, fmt.Errorf("switch rejected: %w", errDaemonStopping)
	}
	old := c.session
	c.session = newS
	c.config = nextCfg
	c.mu.Unlock()

	// g) close old session after swap (so tunToServer sees new session)
	if old != nil {
		old.Close()
	}

	if !c.publishSwitchedSession(newS) {
		return nil, fmt.Errorf("switch rejected: %w", errDaemonStopping)
	}

	log.Printf("[SWITCH] Switched to %s:%d session=%d", nextCfg.Server, nextCfg.Port, newS.id)
	return tunCfg, nil
}

// publishSwitchedSession is the final switch boundary. It serializes its
// stopped-state publication and loop launch with Close, without holding c.mu
// while starting goroutines.
func (c *Client) publishSwitchedSession(newS *Session) bool {
	if c.beforeFinalReady != nil {
		c.beforeFinalReady()
	}
	c.lifecycleMu.Lock()
	published := c.publishReadyAndConflicts(nil)
	if published {
		c.startSessionLoops(newS)
		c.startPacketPumpOnce()
	}
	c.lifecycleMu.Unlock()
	if !published {
		c.failSession(newS, errDaemonStopping)
	}
	return published
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

// SetTUNCleanup stores a TUN/route cleanup function to be called during Close.
// tryStartup calls this once the tunnel is fully established.
//
// Must be safe against racing Close(): if Close() already executed (isStopped),
// the fn is called immediately and not stored (avoids double-cleanup).
// The isStopped check is done atomically with the store under tunCleanupMu so
// that Close() either sees the stored fn (and runs it) or does not see it
// (because SetTUNCleanup already ran it).
func (c *Client) SetTUNCleanup(fn func()) {
	c.tunCleanupMu.Lock()
	stopped := c.isStopped()
	if !stopped {
		c.tunCleanupFn = fn
	}
	c.tunCleanupMu.Unlock()
	if stopped && fn != nil {
		fn()
	}
}

// runTUNCleanup takes and clears the registered cleanup before invoking it so
// external route/device operations never run while tunCleanupMu is held.
func (c *Client) runTUNCleanup() {
	c.tunCleanupMu.Lock()
	fn := c.tunCleanupFn
	c.tunCleanupFn = nil
	c.tunCleanupMu.Unlock()
	if fn != nil {
		fn()
	}
}

// Close cleans up resources. Safe to call multiple times.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		c.lifecycleMu.Lock()
		c.mu.Lock()
		c.stopped = true
		c.tunnelReady.Store(false)
		c.mu.Unlock()
		c.lifecycleMu.Unlock()
		close(c.stopCh)
		c.closeSession()
		c.lifecycleWG.Wait()

		c.runTUNCleanup()
	})
}
