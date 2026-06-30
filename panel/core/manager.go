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

	"github.com/fsnotify/fsnotify"
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
	probeTrigger    chan struct{} // triggers an immediate probe
	probePaused     atomic.Bool   // true = probes suspended (panel hidden)
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
			exeDir:        dir,
			configPath:    filepath.Join(dir, "config.json"),
			iwanPath:      filepath.Join(dir, "iwan.conf"),
			config:        defaultConfig(),
			serverLatency: make(map[string]int64),
			controlAddr:   "127.0.0.1:17890",
			tokenPath:     filepath.Join(dir, "control.token"),
			stopCh:        make(chan struct{}),
			probeTrigger:  make(chan struct{}, 1),
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

// GetStatus returns the current connection state, preferring the daemon's
// control API when available. Snapshot fields under lock, make the HTTP
// call outside the lock, then reacquire to update m.connected.
func (m *SdwanManager) GetStatus() map[string]interface{} {
	m.mu.Lock()
	token := m.token
	controlAddr := m.controlAddr
	hasDaemonCmd := m.daemonCmd != nil && m.daemonCmd.Process != nil
	m.mu.Unlock()

	// Refresh connected state from API if we have a token
	if token != "" {
		sr, err := getControlStatus(controlAddr, token)
		m.mu.Lock()
		defer m.mu.Unlock()
		if err == nil {
			m.connected = sr.State == "running"
		} else if !hasDaemonCmd {
			// API unavailable — only go false if we don't own a process
			m.connected = false
		}
	} else {
		m.mu.Lock()
		defer m.mu.Unlock()
		if !hasDaemonCmd {
			m.connected = false
		}
	}

	return map[string]interface{}{
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

// ToggleConnection starts or stops the tunnel. There is no disconnect API
// yet, so we only ensure the daemon is running when trying to connect.
func (m *SdwanManager) ToggleConnection() bool {
	m.manualChangeSeq.Add(1)
	m.mu.Lock()
	wasConnected := m.connected
	m.mu.Unlock()

	if wasConnected {
		// No disconnect API — leave daemon running
		log.Println("[PANEL] No disconnect API; leaving daemon running")
		return true
	}

	// Start daemon asynchronously — do not hold mu during IO
	go func() {
		if ok := m.ensureDaemonRunning(); ok && m.onStateChange != nil {
			m.onStateChange()
		}
	}()
	return m.isConnected()
}

// SelectServer sets the active server. If connected, uses the daemon's
// POST /v1/switch API. If disconnected, updates config and starts the daemon.
//
// Locking: the validation and snapshot reads happen under mu. The switch API
// call and daemon start happen without the lock. Persistence (config.json +
// iwan.conf) only happens AFTER a successful switch, so failed attempts
// preserve the previous selection.
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

	// Same server — if disconnected, ensure daemon
	if m.config.CurrentServer == id {
		wasConnected := m.connected
		m.mu.Unlock()
		if !wasConnected {
			go m.ensureDaemonRunning()
		}
		return true
	}

	// Switching to a different server — take snapshots, release lock
	wasConnected := m.connected
	token := m.token
	controlAddr := m.controlAddr
	m.mu.Unlock()

	if wasConnected && token != "" {
		// --- API switch path ---
		// Call switch FIRST; only persist on success.
		log.Printf("[PANEL] Switching daemon to %s", targetName)
		if _, err := postControlSwitch(controlAddr, token, targetName); err != nil {
			log.Printf("[PANEL] Daemon switch failed: %v", err)
			m.mu.Lock()
			m.connected = false
			m.mu.Unlock()
			return false
		}

		// Success — persist the new selection
		m.mu.Lock()
		m.config.CurrentServer = id
		m.mu.Unlock()
		_ = m.saveConfig()
		_ = m.syncIwanConf()
	} else {
		// --- Disconnected path ---
		// Persist first, then start daemon.
		m.mu.Lock()
		m.config.CurrentServer = id
		m.mu.Unlock()
		_ = m.saveConfig()
		_ = m.syncIwanConf()

		go m.ensureDaemonRunning()
	}

	return true
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
	if m.connected {
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

// NeedsRestart returns true if the server in iwan.conf differs from the
// server currently running. Used by WatchIwanConf to avoid restarting the
// tunnel for unrelated config edits (MTU, password, etc.).
func (m *SdwanManager) NeedsRestart() bool {
	current := m.ParseIwanServer()
	if current == "" {
		return false
	}
	cfgServer := m.getCurrentServerName()
	return current != cfgServer
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
	select {
	case m.stopCh <- struct{}{}:
	default:
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Keep daemon running — it owns the TUN adapter lifecycle now.
	// Do NOT delete iwan1; daemon handles adapter independently.
	log.Println("[PANEL] Shutdown — leaving daemon running")
}

func (m *SdwanManager) isConnected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connected
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
username=wantl
password=Minieye@2026
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

// stopDaemon kills the daemon subprocess if we own it (panel started it).
func (m *SdwanManager) stopDaemon() {
	if m.daemonCmd == nil || m.daemonCmd.Process == nil {
		return
	}

	pid := m.daemonCmd.Process.Pid
	taskkill := hiddenCommand("taskkill", "/PID", fmt.Sprintf("%d", pid))
	if err := taskkill.Run(); err != nil {
		log.Printf("[DAEMON] taskkill failed, force killing: %v", err)
		m.daemonCmd.Process.Kill()
	}

	log.Printf("[DAEMON] Stopped daemon (PID: %d)", pid)
	m.daemonCmd = nil

	if m.logFile != nil {
		m.logFile.Close()
		m.logFile = nil
	}
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
		if err == nil && sr.State == "running" {
			m.mu.Lock()
			m.connected = true
			m.daemonStarting = false
			m.mu.Unlock()
			return true
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
			m.connected = true
			m.daemonStarting = false
			m.mu.Unlock()
			return true
		}
	}
	log.Println("[DAEMON] Daemon did not become ready in time")
	return false
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
	// Probe all servers in parallel for speed
	var wg sync.WaitGroup
	for _, s := range m.config.Servers {
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
	if ms, ok := m.serverLatency[m.config.CurrentServer]; ok {
		if ms > 0 {
			m.latency = ms
		} else {
			m.latency = 0
		}
	} else {
		m.latency = 0
	}
	m.mu.Unlock()

	if m.onStateChange != nil {
		m.onStateChange()
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

// --- iwan.conf file watcher ------------------------------------------

// WatchIwanConf monitors iwan.conf for external changes (e.g. user edits
// with Notepad) and triggers a restart. onChange is called after a
// 500ms debounce to avoid multiple rapid fires.
func (m *SdwanManager) WatchIwanConf(onChange func()) {
	path := m.iwanPath
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("[CORE] Error creating file watcher: %v", err)
		return
	}
	defer watcher.Close()

	if err := watcher.Add(path); err != nil {
		log.Printf("[CORE] Error watching iwan.conf: %v", err)
		return
	}

	log.Printf("[CORE] Watching config: %s", path)

	var debounceTimer *time.Timer
	const debounceDelay = 500 * time.Millisecond

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
				if debounceTimer != nil {
					debounceTimer.Stop()
				}
				debounceTimer = time.AfterFunc(debounceDelay, func() {
					log.Println("[CORE] iwan.conf modified, restarting...")
					onChange()
				})
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[CORE] Watcher error: %v", err)
		}
	}
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
