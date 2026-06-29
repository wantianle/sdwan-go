package core

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
// by driving the real sdwan-windows-amd64.exe subprocess.
type SdwanManager struct {
	mu              sync.Mutex
	exeDir          string
	configPath      string
	iwanPath        string
	config          *Config
	connected       bool
	latency         int64
	serverLatency   map[string]int64 // per-server latency
	cmd             *exec.Cmd
	logFile         *os.File
	stopCh          chan struct{}    // signals the latency probe to stop
	probeTrigger    chan struct{}    // triggers an immediate probe
	probePaused     atomic.Bool      // true = probes suspended (panel hidden)
	onStateChange   func()           // optional callback for UI refresh
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
			stopCh:        make(chan struct{}),
			probeTrigger:  make(chan struct{}, 1),
		}
		m.loadConfig()
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

// GetStatus returns the current connection state.
func (m *SdwanManager) GetStatus() map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if the process is still alive
	if m.connected && m.cmd != nil && m.cmd.Process != nil {
		// Quick non-blocking check without signalling
	}

	return map[string]interface{}{
		"connected":      m.connected,
		"latency":        m.latency,
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

// ToggleConnection starts or stops the sdwan.exe subprocess.
func (m *SdwanManager) ToggleConnection() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.connected {
		m.stopCore()
	} else {
		m.startCore()
	}
	return m.connected
}

// SelectServer sets the active server. If connected, reconnects with the new server.
func (m *SdwanManager) SelectServer(id string) bool {
	m.mu.Lock()

	found := false
	for _, s := range m.config.Servers {
		if s.ID == id {
			found = true
			break
		}
	}
	if !found {
		m.mu.Unlock()
		return false
	}

	wasConnected := m.connected
	if wasConnected {
		m.stopCore()
	}
	m.config.CurrentServer = id
	m.mu.Unlock()

	_ = m.saveConfig()
	_ = m.syncIwanConf()

	if wasConnected {
		m.mu.Lock()
		m.startCore()
		m.mu.Unlock()
	}
	return true
}

// Reload re-reads both config files and restarts sdwan if needed.
func (m *SdwanManager) Reload() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.loadConfig()

	if m.connected {
		m.stopCore()
		m.startCore()
	}
	return true
}

// EditConfig opens iwan.conf with Windows Notepad.
func (m *SdwanManager) EditConfig() error {
	return exec.Command("notepad", m.iwanPath).Start()
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

// Shutdown gracefully stops the core and persists state.
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.connected {
		m.stopCore()
	}
	// Clean up stale wintun adapter so next start is fresh
	exec.Command("wmic", "path", "Win32_NetworkAdapter",
		"where", "NetConnectionID='iwan1'", "delete").Run()
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

// --- real core: process management -----------------------------------

func (m *SdwanManager) startCore() {
	exePath := filepath.Join(m.exeDir, "sdwan-windows-amd64.exe")

	if _, err := os.Stat(exePath); os.IsNotExist(err) {
		log.Printf("[CORE] sdwan.exe not found at %s", exePath)
		m.connected = false
		return
	}

	// Sync iwan.conf before starting
	if err := m.syncIwanConf(); err != nil {
		log.Printf("[CORE] Failed to sync iwan.conf: %v", err)
	}

	// Open log file
	logPath := filepath.Join(m.exeDir, "sdwan.log")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("[CORE] Warning: Could not open log file: %v", err)
	}
	m.logFile = lf

	m.cmd = exec.Command(exePath)
	m.cmd.Dir = m.exeDir
	m.cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	if lf != nil {
		m.cmd.Stdout = lf
		m.cmd.Stderr = lf
	}

	if err := m.cmd.Start(); err != nil {
		log.Printf("[CORE] Failed to start sdwan.exe: %v", err)
		m.connected = false
		if lf != nil {
			lf.Close()
		}
		return
	}

	m.connected = true
	m.stopCh = make(chan struct{})
	log.Printf("[CORE] Started sdwan.exe (PID: %d), server=%s", m.cmd.Process.Pid, m.getCurrentServerName())

	// Monitor process exit
	go func() {
		err := m.cmd.Wait()
		m.mu.Lock()
		wasRunning := m.connected
		m.connected = false
		m.latency = 0
		m.mu.Unlock()

		if err != nil {
			log.Printf("[CORE] sdwan.exe exited with error: %v", err)
		} else {
			log.Println("[CORE] sdwan.exe exited normally")
		}

		if m.logFile != nil {
			m.logFile.Close()
			m.logFile = nil
		}

		// Notify state change even on crash/exit
		if wasRunning && m.onStateChange != nil {
			m.onStateChange()
		}
	}()

	// Start latency probe
	go m.latencyProbe()
}

func (m *SdwanManager) stopCore() {
	if m.cmd == nil || m.cmd.Process == nil {
		m.connected = false
		return
	}

	// Signal latency probe to stop
	select {
	case m.stopCh <- struct{}{}:
	default:
	}

	pid := m.cmd.Process.Pid

	// Try graceful shutdown via taskkill
	taskkill := exec.Command("taskkill", "/PID", fmt.Sprintf("%d", pid))
	if err := taskkill.Run(); err != nil {
		log.Printf("[CORE] taskkill failed, force killing: %v", err)
		m.cmd.Process.Kill()
	}

	m.connected = false
	m.latency = 0

	if m.logFile != nil {
		m.logFile.Close()
		m.logFile = nil
	}

	log.Printf("[CORE] Stopped sdwan.exe (PID: %d)", pid)
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
				m.serverLatency[sid] = lat
			}
			m.mu.Unlock()
		}(s.ID, s.Name)
	}
	wg.Wait()

	// Update current server latency for status header
	m.mu.Lock()
	if ms, ok := m.serverLatency[m.config.CurrentServer]; ok {
		m.latency = ms
	}
	m.mu.Unlock()

	if m.onStateChange != nil {
		m.onStateChange()
	}
}

// formatLatency converts a latency value to a display string.
func formatLatency(ms int64) string {
	if ms <= 0 {
		return ""
	}
	if ms < 1 {
		return "<1ms"
	}
	return fmt.Sprintf("%dms", ms)
}

// probeLatency uses TCP dial to port 443 to measure real RTT.
// TCP handshake gives authentic round-trip time — no output parsing needed.
func probeLatency(server string) int64 {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(server, "443"), 2*time.Second)
	if err != nil {
		log.Printf("[LATENCY] %s:443 unreachable: %v", server, err)
		return -1
	}
	conn.Close()
	ms := time.Since(start).Milliseconds()
	log.Printf("[LATENCY] %s:443 = %dms", server, ms)
	return ms
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
