package core

import (
	"bufio"
	"crypto/aes"
	"crypto/md5"
	"encoding/json"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	autoConnecting  atomic.Bool
	manualChangeSeq atomic.Uint64
	lastAutoAttempt atomic.Int64
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

// ToggleConnection starts or stops the sdwan.exe subprocess.
func (m *SdwanManager) ToggleConnection() bool {
	m.manualChangeSeq.Add(1)
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
	m.manualChangeSeq.Add(1)
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

// AutoConnect attempts one bounded startup connection pass.
// Order: current server first, then remaining servers by lowest known latency,
// falling back to config order for unknown latencies.
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

	go func(seq uint64) {
		defer m.autoConnecting.Store(false)

		if m.isManualSequenceChanged(seq) || m.isConnected() {
			return
		}

		m.probeOnce()
		candidates := m.autoConnectCandidates()
		for _, id := range candidates {
			if m.isManualSequenceChanged(seq) || m.isConnected() {
				return
			}
			if !m.tryAutoConnectCandidate(id) {
				continue
			}
			if m.waitForStableConnection(8 * time.Second, seq) {
				log.Printf("[AUTO] Connected using server %s", id)
				return
			}
		}
		log.Println("[AUTO] No server established a stable startup connection")
	}(m.manualChangeSeq.Load())
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
	if m.connected {
		m.stopCore()
	}
	// Clean up stale wintun adapter so next start is fresh
	hiddenCommand("wmic", "path", "Win32_NetworkAdapter",
		"where", "NetConnectionID='iwan1'", "delete").Run()
}

func (m *SdwanManager) isConnected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connected
}

func (m *SdwanManager) isManualSequenceChanged(seq uint64) bool {
	return m.manualChangeSeq.Load() != seq
}

func (m *SdwanManager) autoConnectCandidates() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	type candidate struct {
		id      string
		latency int64
		index   int
	}

	current := m.config.CurrentServer
	remaining := make([]candidate, 0, len(m.config.Servers))
	ordered := make([]string, 0, len(m.config.Servers))

	for i, s := range m.config.Servers {
		if s.ID == current {
			ordered = append(ordered, s.ID)
			continue
		}
		remaining = append(remaining, candidate{id: s.ID, latency: m.serverLatency[s.ID], index: i})
	}

	sort.SliceStable(remaining, func(i, j int) bool {
		li, lj := remaining[i].latency, remaining[j].latency
		ki := li > 0
		kj := lj > 0
		if ki != kj {
			return ki
		}
		if ki && lj != li {
			return li < lj
		}
		return remaining[i].index < remaining[j].index
	})

	for _, c := range remaining {
		ordered = append(ordered, c.id)
	}
	return ordered
}

func (m *SdwanManager) tryAutoConnectCandidate(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.connected {
		return true
	}

	found := false
	for _, s := range m.config.Servers {
		if s.ID == id {
			found = true
			break
		}
	}
	if !found {
		return false
	}

	m.config.CurrentServer = id
	if err := m.saveConfig(); err != nil {
		log.Printf("[AUTO] Failed to save config for server %s: %v", id, err)
	}
	if err := m.syncIwanConf(); err != nil {
		log.Printf("[AUTO] Failed to sync iwan.conf for server %s: %v", id, err)
	}
	log.Printf("[AUTO] Trying server %s (%s)", id, m.getCurrentServerName())
	m.startCore()
	return m.connected
}

func (m *SdwanManager) waitForStableConnection(timeout time.Duration, seq uint64) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if m.isManualSequenceChanged(seq) {
			return false
		}
		if !m.isConnected() {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
	return m.isConnected() && !m.isManualSequenceChanged(seq)
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

	m.cmd = hiddenCommand(exePath)
	m.cmd.Dir = m.exeDir

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

}

func (m *SdwanManager) stopCore() {
	if m.cmd == nil || m.cmd.Process == nil {
		m.connected = false
		return
	}

	pid := m.cmd.Process.Pid

	// Try graceful shutdown via taskkill
	taskkill := hiddenCommand("taskkill", "/PID", fmt.Sprintf("%d", pid))
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
			m.serverLatency[sid] = lat
			m.mu.Unlock()
		}(s.ID, s.Name)
	}
	wg.Wait()

	// Update current server latency for status header
	m.mu.Lock()
	if ms, ok := m.serverLatency[m.config.CurrentServer]; ok {
		m.latency = ms
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
