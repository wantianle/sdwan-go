//go:build windows

package core

import (
	"bufio"
	"crypto/aes"
	"crypto/md5"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ServerInfo represents a selectable SD-WAN server node.
type ServerInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Config represents the sdwan-panel configuration file (config.json).
type Config struct {
	CurrentServer string       `json:"current_server"`
	Servers       []ServerInfo `json:"servers"`
}

// SdwanManager is a singleton that manages the SD-WAN tunnel lifecycle
// by supervising the sdwan-windows-amd64.exe daemon process and driving
// server selection through the daemon's HTTP control API.
type SdwanManager struct {
	mu              sync.Mutex
	exeDir          string
	configPath      string
	iwanPath        string
	config          *Config
	state           string
	connected       bool
	latency         int64
	serverLatency   map[string]int64 // per-server latency
	daemonCmd       *exec.Cmd        // the daemon subprocess
	daemonStarting  bool             // true while ensureDaemonRunning is launching
	logFile         *os.File
	controlAddr     string        // "127.0.0.1:17890"
	tokenPath       string        // path to control.token
	token           string        // loaded bearer token
	stopCh          chan struct{} // signals poller / latency probe to stop
	stopOnce        sync.Once
	probeTrigger    chan struct{} // triggers an immediate probe
	probePaused     atomic.Bool   // true = probes suspended (panel hidden)
	daemonPollStop  chan struct{}
	daemonPollerOn  atomic.Bool
	autoConnecting  atomic.Bool
	manualChangeSeq atomic.Uint64
	lastAutoAttempt atomic.Int64
	onStateChange   func() // optional callback for UI refresh
}

var instance *SdwanManager
var once sync.Once

// GetManager returns the singleton SdwanManager, initialised with config
// files located in the same directory as the executable.
func GetManager() *SdwanManager {
	once.Do(func() {
		exe, _ := os.Executable()
		dir := filepath.Dir(exe)

		m := &SdwanManager{
			exeDir:         dir,
			configPath:     filepath.Join(dir, "config.json"),
			iwanPath:       filepath.Join(dir, "iwan.conf"),
			config:         defaultConfig(),
			state:          "disconnected",
			serverLatency:  make(map[string]int64),
			controlAddr:    "127.0.0.1:17890",
			tokenPath:      filepath.Join(dir, "control.token"),
			stopCh:         make(chan struct{}),
			probeTrigger:   make(chan struct{}, 1),
			daemonPollStop: make(chan struct{}, 1),
		}
		// Generates token on first install so panel and daemon share one.
		if tok, err := loadOrGenerateToken(m.tokenPath); err == nil {
			m.token = tok
		} else {
			log.Printf("[PANEL] Token init failed: %v", err)
		}
		m.loadConfig()
		go m.latencyProbe()
		instance = m
	})
	return instance
}

// SetStateChangeCallback registers a function to be called whenever the
// connection state changes (process started / stopped / crashed).
func (m *SdwanManager) SetStateChangeCallback(fn func()) {
	m.mu.Lock()
	m.onStateChange = fn
	m.mu.Unlock()
}

func defaultConfig() *Config {
	return &Config{
		CurrentServer: "1",
		Servers: []ServerInfo{
			{ID: "1", Name: "minieye.9966.org"},
			{ID: "2", Name: "dwan.minieye.tech"},
			{ID: "3", Name: "minieye.8866.org"},
			{ID: "4", Name: "minieye.2288.org"},
			{ID: "5", Name: "youjia.8866.org"},
		},
	}
}

func (m *SdwanManager) loadConfig() {
	data, err := os.ReadFile(m.configPath)
	if err != nil {
		return
	}
	var cfg Config
	if json.Unmarshal(data, &cfg) != nil {
		return
	}
	if len(cfg.Servers) > 0 {
		m.config = &cfg
	}
}

func (m *SdwanManager) saveConfig() error {
	data, err := json.MarshalIndent(m.config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.configPath, data, 0644)
}

// GetStatus returns the cached connection state. It intentionally avoids
// synchronous control API calls so frontend polling never blocks the Wails
// bridge when the daemon is unreachable.
func (m *SdwanManager) GetStatus() map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()

	return map[string]interface{}{
		"state":          m.state,
		"connected":      m.connected,
		"latency":        m.latency,
		"latency_text":   formatLatency(m.latency),
		"current_server": m.getCurrentServerName(),
	}
}

// GetServers returns the configured server list.
func (m *SdwanManager) GetServers() []map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()

	list := make([]map[string]string, 0, len(m.config.Servers))
	for _, s := range m.config.Servers {
		sel := "false"
		if s.ID == m.config.CurrentServer {
			sel = "true"
		}
		list = append(list, map[string]string{
			"id":       s.ID,
			"name":     s.Name,
			"selected": sel,
			"latency":  formatLatency(m.serverLatency[s.ID]),
		})
	}
	return list
}

// ToggleConnection checks the daemon's control API status. If the API is
// reachable but the tunnel is disconnected, it reconnects via POST /v1/switch.
// If the API is unreachable, it starts the daemon.
func (m *SdwanManager) ToggleConnection() bool {
	m.manualChangeSeq.Add(1)
	m.mu.Lock()
	token := m.token
	controlAddr := m.controlAddr
	state := m.state
	cached := m.connected
	m.mu.Unlock()

	go func() {
		if token == "" {
			if ok := m.ensureDaemonRunning(); ok && m.onStateChange != nil {
				m.onStateChange()
			}
			return
		}

		if state == "running" || state == "reconnecting" {
			if err := postControlPause(controlAddr, token, true); err != nil {
				log.Printf("[PANEL] Pause failed: %v", err)
				if m.onStateChange != nil {
					m.onStateChange()
				}
				return
			}
			m.mu.Lock()
			m.state = "paused"
			m.connected = false
			m.mu.Unlock()
			if m.onStateChange != nil {
				m.onStateChange()
			}
			return
		}

		sr, err := getControlStatus(controlAddr, token)
		if err != nil {
			if ok := m.ensureDaemonRunning(); ok && m.onStateChange != nil {
				m.onStateChange()
			}
			return
		}
		m.startDaemonPoller()

		if sr.State == "running" {
			m.mu.Lock()
			m.state = "running"
			m.connected = true
			m.mu.Unlock()
			if m.onStateChange != nil {
				m.onStateChange()
			}
			return
		}
		m.mu.Lock()
		m.state = sr.State
		m.connected = false
		m.mu.Unlock()

		log.Println("[PANEL] Triggering daemon reconnect")
		if err := postControlPause(controlAddr, token, false); err != nil {
			log.Printf("[PANEL] Resume failed: %v", err)
			m.mu.Lock()
			m.state = "disconnected"
			m.connected = false
			m.mu.Unlock()
		} else {
			m.mu.Lock()
			m.state = "reconnecting"
			m.connected = false
			m.mu.Unlock()
		}
		if m.onStateChange != nil {
			m.onStateChange()
		}
	}()

	return cached
}

// SelectServer sets the active server. Prefers the daemon's control API
// reachability over cached m.connected: if the API responds (even if the
// tunnel is disconnected), a switch is attempted. Only falls back to
// start-daemon when the API is completely unreachable.
//
// Persistence (config.json + iwan.conf) only happens AFTER a successful
// switch, so failed attempts preserve the previous selection.
func (m *SdwanManager) SelectServer(id string) bool {
	m.manualChangeSeq.Add(1)
	m.mu.Lock()

	found := false
	targetName := ""
	for _, s := range m.config.Servers {
		if s.ID == id {
			found = true
			targetName = s.Name
			break
		}
	}
	if !found {
		m.mu.Unlock()
		return false
	}

	isSameServer := m.config.CurrentServer == id
	token := m.token
	controlAddr := m.controlAddr
	m.mu.Unlock()

	if token != "" {
		sr, err := getControlStatus(controlAddr, token)
		if err == nil {
			m.startDaemonPoller()
			m.mu.Lock()
			m.state = sr.State
			m.connected = sr.State == "running"
			m.mu.Unlock()

			// Same server and already running → no-op.
			if isSameServer && sr.State == "running" {
				if m.onStateChange != nil {
					m.onStateChange()
				}
				return true
			}

			log.Printf("[PANEL] Switching daemon to %s", targetName)
			if _, err := postControlSwitch(controlAddr, token, targetName); err != nil {
				log.Printf("[PANEL] Daemon switch failed: %v", err)
				m.mu.Lock()
				m.state = "disconnected"
				m.connected = false
				m.mu.Unlock()
				if m.onStateChange != nil {
					m.onStateChange()
				}
				return false
			}

			m.mu.Lock()
			m.config.CurrentServer = id
			m.state = "running"
			m.connected = true
			m.mu.Unlock()
			_ = m.saveConfig()
			if !isSameServer {
				_ = m.syncIwanConf()
			}
			if m.onStateChange != nil {
				m.onStateChange()
			}
			return true
		}
	}

	// --- API unreachable ---
	// Persist first, then start daemon. For same-server no-op, only try
	// to ensure daemon.
	if !isSameServer {
		m.mu.Lock()
		m.config.CurrentServer = id
		m.mu.Unlock()
		_ = m.saveConfig()
		_ = m.syncIwanConf()
	}
	ok := m.ensureDaemonRunning()
	if m.onStateChange != nil {
		m.onStateChange()
	}
	return ok
}

// Reload re-reads config.json.
func (m *SdwanManager) Reload() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.loadConfig()
	return true
}

// AutoConnect ensures the daemon is running and connected on the configured
// server. This is called on panel startup and when the panel is shown.
func (m *SdwanManager) AutoConnect() {
	m.mu.Lock()
	connected := m.connected
	m.mu.Unlock()
	if connected {
		return
	}
	now := time.Now().UnixNano()
	last := m.lastAutoAttempt.Load()
	if last != 0 && now-last < int64(30*time.Second) {
		return
	}
	if !m.lastAutoAttempt.CompareAndSwap(last, now) && m.lastAutoAttempt.Load() != now {
		return
	}
	if !m.autoConnecting.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer m.autoConnecting.Store(false)
		m.probeOnce()
		ok := m.ensureDaemonRunning()
		if ok && m.onStateChange != nil {
			m.onStateChange()
		}
	}()
}

// EditConfig opens iwan.conf with Windows Notepad.
func (m *SdwanManager) EditConfig() error {
	return exec.Command("notepad", m.iwanPath).Start()
}

func hiddenCommand(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd
}

// ResumeProbes unpauses the latency probe and fires an immediate probe cycle.
func (m *SdwanManager) ResumeProbes() {
	m.probePaused.Store(false)
	select {
	case m.probeTrigger <- struct{}{}:
	default:
	}
}

// SuspendProbes pauses all latency probing (panel hidden).
func (m *SdwanManager) SuspendProbes() {
	m.probePaused.Store(true)
}

func (m *SdwanManager) Shutdown() {
	// Signal the daemon to exit gracefully before stopping probes.
	// The daemon runs existing defers: route delete, TUN close, adapter cleanup.
	m.mu.Lock()
	token := m.token
	controlAddr := m.controlAddr
	m.mu.Unlock()

	if token != "" {
		log.Println("[PANEL] Sending shutdown to daemon...")
		var err error
		for i := 0; i < 5; i++ {
			err = postControlShutdown(controlAddr, token)
			if err == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if err != nil {
			log.Printf("[PANEL] Daemon shutdown request failed after retries: %v", err)
		}
		// Small settle so the daemon has time to begin cleanup.
		time.Sleep(500 * time.Millisecond)
	}

	m.stopOnce.Do(func() { close(m.stopCh) })
	select {
	case m.daemonPollStop <- struct{}{}:
	default:
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	log.Println("[PANEL] Shutdown — probes stopped, daemon shutting down")
}

func (m *SdwanManager) isManualSequenceChanged(seq uint64) bool {
	return m.manualChangeSeq.Load() != seq
}

// --- iwan.conf sync -------------------------------------------------

// syncIwanConf reads the existing iwan.conf and updates the server= line.
// Other fields (username, password, port, mtu, encrypt, etc.) are preserved.
func (m *SdwanManager) syncIwanConf() error {
	serverName := m.getCurrentServerName()

	// Skip if already correct — prevents watcher→reload→sync→watcher loop
	if m.ParseIwanServer() == serverName {
		return nil
	}

	// Read existing iwan.conf
	data, err := os.ReadFile(m.iwanPath)
	if err != nil {
		return m.writeDefaultIwanConf()
	}

	lines := strings.Split(string(data), "\n")
	found := false
	newLine := "server=" + serverName

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "server=") || strings.HasPrefix(trimmed, "server ") {
			lines[i] = newLine
			found = true
			break
		}
	}

	if !found {
		lines = append(lines, newLine)
	}

	return os.WriteFile(m.iwanPath, []byte(strings.Join(lines, "\n")), 0644)
}

func (m *SdwanManager) writeDefaultIwanConf() error {
	serverName := m.getCurrentServerName()
	content := fmt.Sprintf(`server=%s
port=10010
username=
password=
mtu=1436
encrypt=0
tunname=iwan1
routenet=192.168.0.0/16
`, serverName)
	return os.WriteFile(m.iwanPath, []byte(content), 0644)
}

// --- daemon supervisor -------------------------------------------------

// startDaemon launches sdwan-windows-amd64.exe in daemon mode (-daemon).
// It does NOT block or wait for the daemon to become ready; callers should
// use ensureDaemonRunning for that.
func (m *SdwanManager) startDaemon() {
	exePath := filepath.Join(m.exeDir, "sdwan-windows-amd64.exe")

	if _, err := os.Stat(exePath); os.IsNotExist(err) {
		log.Printf("[DAEMON] sdwan-windows-amd64.exe not found at %s", exePath)
		return
	}

	// Sync iwan.conf before starting daemon
	if err := m.syncIwanConf(); err != nil {
		log.Printf("[DAEMON] Failed to sync iwan.conf: %v", err)
	}

	// Open log file for daemon stdout/stderr
	logPath := filepath.Join(m.exeDir, "sdwan.log")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("[DAEMON] Warning: Could not open log file: %v", err)
	}
	m.logFile = lf

	m.daemonCmd = hiddenCommand(exePath,
		"-daemon",
		"-f", m.iwanPath,
		"-control", m.controlAddr,
		"-token-file", m.tokenPath,
	)
	m.daemonCmd.Dir = m.exeDir

	if lf != nil {
		m.daemonCmd.Stdout = lf
		m.daemonCmd.Stderr = lf
	}

	if err := m.daemonCmd.Start(); err != nil {
		log.Printf("[DAEMON] Failed to start daemon: %v", err)
		m.daemonCmd = nil
		if lf != nil {
			lf.Close()
		}
		return
	}

	log.Printf("[DAEMON] Started daemon (PID: %d)", m.daemonCmd.Process.Pid)

	// Capture locals so the monitor goroutine does not reference the
	// mutable m.daemonCmd / m.logFile fields after unlock.
	cmd := m.daemonCmd
	lf = m.logFile

	// Monitor process exit in background
	go func() {
		_ = cmd.Wait()
		m.mu.Lock()
		wasRunning := m.connected
		// Only clear daemonCmd if it's still this instance (not replaced by a
		// second start).
		if m.daemonCmd == cmd {
			m.daemonCmd = nil
		}
		m.daemonStarting = false
		m.mu.Unlock()

		if lf != nil {
			lf.Close()
			m.mu.Lock()
			// Only clear logFile if it hasn't been replaced.
			if m.logFile == lf {
				m.logFile = nil
			}
			m.mu.Unlock()
		}

		if wasRunning && m.onStateChange != nil {
			m.onStateChange()
		}
	}()
}

// ensureDaemonRunning checks whether the daemon is reachable via its control
// API. If not, it starts the daemon and polls the API until ready (bounded).
//
// This method does NOT hold m.mu while making HTTP calls or starting
// processes. It briefly locks to read/write m.connected, m.daemonCmd, and
// m.daemonStarting.
//
// Returns true if the daemon is confirmed running via its control API.
//
// Uses a double-checked lock pattern to prevent duplicate daemon starts:
// the second check (under lock, just before setting daemonStarting=true)
// falls through to the polling path if another goroutine already started.
func (m *SdwanManager) ensureDaemonRunning() bool {
	// Take snapshots outside the lock so we don't hold mu during IO.
	m.mu.Lock()
	token := m.token
	controlAddr := m.controlAddr
	alreadyStarted := m.daemonCmd != nil || m.daemonStarting
	m.mu.Unlock()

	// Quick check: is API already responding?
	if token != "" {
		sr, err := getControlStatus(controlAddr, token)
		if err == nil {
			if sr.State == "running" {
				m.mu.Lock()
				m.state = "running"
				m.connected = true
				m.daemonStarting = false
				m.mu.Unlock()
				m.startDaemonPoller()
				return true
			}
			// API reachable but tunnel disconnected — daemon process is alive,
			// just needs a reconnection via /v1/switch. Do NOT start a duplicate.
			m.mu.Lock()
			m.state = sr.State
			m.connected = false
			m.daemonStarting = false
			m.mu.Unlock()
			m.startDaemonPoller()
			return false
		}
		// If API returned 401, the token is wrong → don't start a daemon
		// that would generate another (mismatched) token.
		if isAuthError(err) {
			log.Printf("[DAEMON] Token/auth mismatch — not starting duplicate daemon")
			m.mu.Lock()
			m.daemonStarting = false
			m.mu.Unlock()
			return false
		}
	}

	// Guard: if a daemon is already running or starting, just poll.
	if alreadyStarted {
		return m.pollDaemonReady(token, controlAddr)
	}

	// Second check under lock: re-verify no one else started while we were
	// doing the initial API quick-check above.
	m.mu.Lock()
	if m.daemonCmd != nil || m.daemonStarting {
		m.mu.Unlock()
		return m.pollDaemonReady(token, controlAddr)
	}

	m.daemonStarting = true
	m.startDaemon()
	if m.daemonCmd == nil {
		// startDaemon failed (binary missing, etc.)
		m.daemonStarting = false
		m.mu.Unlock()
		return false
	}
	m.mu.Unlock()

	// Poll API for up to 20 seconds
	if ok := m.pollDaemonReady(token, controlAddr); ok {
		return true
	}

	m.mu.Lock()
	m.daemonStarting = false
	m.mu.Unlock()
	return false
}

// pollDaemonReady blocks for up to 20 seconds polling the daemon's control
// API. It acquires m.mu only briefly to update m.connected.
func (m *SdwanManager) pollDaemonReady(token, controlAddr string) bool {
	if token == "" {
		return false
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(1 * time.Second)
		if sr, err := getControlStatus(controlAddr, token); err == nil && sr.State == "running" {
			m.mu.Lock()
			m.state = "running"
			m.connected = true
			m.daemonStarting = false
			m.mu.Unlock()
			m.startDaemonPoller()
			return true
		}
	}
	log.Println("[DAEMON] Daemon did not become ready in time")
	return false
}

// startDaemonPoller keeps cached daemon state fresh in the background so
// Wails-facing methods can return immediately without synchronous HTTP calls.
func (m *SdwanManager) startDaemonPoller() {
	if !m.daemonPollerOn.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer m.daemonPollerOn.Store(false)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		m.pollDaemonStatusOnce()
		for {
			select {
			case <-m.daemonPollStop:
				return
			case <-ticker.C:
				m.pollDaemonStatusOnce()
			}
		}
	}()
}

func (m *SdwanManager) pollDaemonStatusOnce() {
	m.mu.Lock()
	token := m.token
	controlAddr := m.controlAddr
	hasDaemonCmd := m.daemonCmd != nil && m.daemonCmd.Process != nil
	wasState := m.state
	m.mu.Unlock()

	if token == "" {
		return
	}
	sr, err := getControlStatus(controlAddr, token)

	m.mu.Lock()
	if err == nil {
		m.state = sr.State
		m.connected = sr.State == "running"
	} else if !hasDaemonCmd {
		m.state = "disconnected"
		m.connected = false
	}
	changed := wasState != m.state
	m.mu.Unlock()

	if changed && m.onStateChange != nil {
		m.onStateChange()
	}
}

// --- latency probe ---------------------------------------------------

// latencyProbe periodically checks server latency via TCP dial.
// Probes are suspended when SuspendProbes() is called (panel hidden).
func (m *SdwanManager) latencyProbe() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-m.probeTrigger:
			if !m.probePaused.Load() {
				m.probeOnce()
			}
		case <-ticker.C:
			if !m.probePaused.Load() {
				m.probeOnce()
			}
		}
	}
}

func (m *SdwanManager) probeOnce() {
	// Snapshot fields under lock to avoid races with config updates.
	m.mu.Lock()
	servers := make([]ServerInfo, len(m.config.Servers))
	copy(servers, m.config.Servers)
	currentServer := m.config.CurrentServer
	stateChange := m.onStateChange
	m.mu.Unlock()

	// Probe all servers in parallel for speed
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		go func(sid, sname string) {
			defer wg.Done()
			lat := probeLatency(sname)
			m.mu.Lock()
			if lat > 0 {
				m.serverLatency[sid] = smoothLatency(m.serverLatency[sid], lat)
			} else if existing := m.serverLatency[sid]; existing > 0 {
				log.Printf("[LATENCY] %s probe failed, keeping last good latency %dms", sname, existing)
			} else {
				m.serverLatency[sid] = 0
			}
			m.mu.Unlock()
		}(s.ID, s.Name)
	}
	wg.Wait()

	// Update current server latency for status header
	m.mu.Lock()
	if ms, ok := m.serverLatency[currentServer]; ok {
		if ms > 0 {
			m.latency = ms
		} else {
			m.latency = 0
		}
	} else {
		m.latency = 0
	}
	m.mu.Unlock()

	if stateChange != nil {
		stateChange()
	}
}

// formatLatency converts a latency value to a display string.
func formatLatency(ms int64) string {
	if ms < 0 {
		return "timeout/unreachable"
	}
	if ms == 0 {
		return "--"
	}
	if ms < 1 {
		return "<1ms"
	}
	return fmt.Sprintf("%dms", ms)
}

func smoothLatency(previous, sample int64) int64 {
	if sample <= 0 {
		return previous
	}
	if previous <= 0 {
		return sample
	}
	return (previous*7 + sample*3 + 5) / 10
}

// NOTE: The probe protocol functions below (probeConfig, probeMsgOPENACK,
// probeMsgOPEN, probeLatency, loadProbeConfig, probePktSign,
// probePktSignInPlace, probePktVerify, probeEncryptPassword,
// buildProbeOpenPacket, probeMsgType) are duplicates of
// internal/core/protocol.go and must be kept in sync with any changes there.
// They are actively used by the latency probe (probeOnce) and cannot be
// removed / replaced with direct imports due to the Windows-only build
// constraint of this package.

type probeConfig struct {
	Server   string
	Username string
	Password string
	Port     int
	MTU      int
	Encrypt  int
}

const (
	probeMsgOPENACK byte = 0x12
	probeMsgOPEN    byte = 0x13
)

// probeLatency sends the SD-WAN OPEN handshake over UDP:10010 and measures
// the time until a valid OPENACK arrives.
func probeLatency(server string) int64 {
	cfg := loadProbeConfig(server)
	if cfg.Username == "" || cfg.Password == "" {
		log.Printf("[LATENCY] %s probe skipped: missing username/password in iwan.conf", server)
		return -1
	}

	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(cfg.Server, strconv.Itoa(cfg.Port)))
	if err != nil {
		log.Printf("[LATENCY] %s:%d resolve failed: %v", cfg.Server, cfg.Port, err)
		return -1
	}

	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		log.Printf("[LATENCY] %s:%d dial failed: %v", cfg.Server, cfg.Port, err)
		return -1
	}
	defer conn.Close()

	openPkt := buildProbeOpenPacket(cfg)
	buf := make([]byte, 2048)
	start := time.Now()
	if _, err := conn.Write(openPkt); err != nil {
		log.Printf("[LATENCY] %s:%d send OPEN failed: %v", cfg.Server, cfg.Port, err)
		return -1
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		n, err := conn.Read(buf)
		if err != nil {
			log.Printf("[LATENCY] %s:%d OPENACK timeout/unreachable: %v", cfg.Server, cfg.Port, err)
			return -1
		}
		data := buf[:n]
		if len(data) < 24 {
			continue
		}
		if probeMsgType(data) != probeMsgOPENACK {
			continue
		}
		if !probePktVerify(data) {
			continue
		}
		ms := time.Since(start).Milliseconds()
		log.Printf("[LATENCY] %s:%d OPEN/OPENACK = %dms", cfg.Server, cfg.Port, ms)
		return ms
	}
}

func loadProbeConfig(server string) probeConfig {
	cfg := probeConfig{
		Server:  server,
		Port:    10010,
		MTU:     1436,
		Encrypt: 0,
	}

	f, err := os.Open(instance.iwanPath)
	if err != nil {
		return cfg
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		switch key {
		case "server":
			if cfg.Server == "" {
				cfg.Server = val
			}
		case "username":
			cfg.Username = val
		case "password":
			cfg.Password = val
		case "port":
			if v, err := strconv.Atoi(val); err == nil && v > 0 {
				cfg.Port = v
			}
		case "mtu":
			if v, err := strconv.Atoi(val); err == nil && v > 0 {
				cfg.MTU = v
			}
		case "encrypt":
			if v, err := strconv.Atoi(val); err == nil {
				cfg.Encrypt = v
			}
		}
	}
	return cfg
}

func probePktSign(header []byte) []byte {
	h := md5.New()
	_, _ = h.Write(header[:8])
	_, _ = h.Write([]byte("mw"))
	return h.Sum(nil)
}

func probePktSignInPlace(header []byte) {
	copy(header[8:24], probePktSign(header))
}

func probePktVerify(header []byte) bool {
	expected := probePktSign(header)
	for i := 0; i < 16; i++ {
		if header[8+i] != expected[i] {
			return false
		}
	}
	return true
}

func probeEncryptPassword(username, password string) []byte {
	h := md5.New()
	_, _ = h.Write([]byte("mw"))
	_, _ = h.Write([]byte(username))
	aesKey := h.Sum(nil)

	block := make([]byte, 16)
	copy(block, password)
	cipher, err := aes.NewCipher(aesKey)
	if err != nil {
		return block
	}
	cipher.Encrypt(block, block)
	return block
}

func buildProbeOpenPacket(cfg probeConfig) []byte {
	buf := make([]byte, 1024)
	pos := 0

	buf[pos] = probeMsgOPEN
	buf[pos+1] = byte(cfg.Encrypt)
	pos += 8
	pos += 16

	buf[pos] = 0x03
	buf[pos+1] = 0x04
	binary.BigEndian.PutUint16(buf[pos+2:pos+4], uint16(cfg.MTU))
	pos += 4

	buf[pos] = 0x01
	buf[pos+1] = byte(len(cfg.Username) + 2)
	copy(buf[pos+2:], cfg.Username)
	pos += 2 + len(cfg.Username)

	encPW := probeEncryptPassword(cfg.Username, cfg.Password)
	buf[pos] = 0x02
	buf[pos+1] = 0x12
	copy(buf[pos+2:], encPW)
	pos += 18

	if cfg.Encrypt != 0 {
		buf[pos] = 0x08
		buf[pos+1] = 0x03
		buf[pos+2] = byte(cfg.Encrypt)
		pos += 3
	}

	pkt := buf[:pos]
	probePktSignInPlace(pkt[:24])
	return pkt
}

func probeMsgType(data []byte) byte {
	if len(data) == 0 {
		return 0
	}
	return data[0]
}

// --- helpers ---------------------------------------------------------

// ParseIwanServer reads the server= field from iwan.conf.
func (m *SdwanManager) ParseIwanServer() string {
	f, err := os.Open(m.iwanPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		if key == "server" {
			return strings.TrimSpace(parts[1])
		}
	}
	return ""
}

func (m *SdwanManager) getCurrentServerName() string {
	for _, s := range m.config.Servers {
		if s.ID == m.config.CurrentServer {
			return s.Name
		}
	}
	return fmt.Sprintf("节点 %s", m.config.CurrentServer)
}
