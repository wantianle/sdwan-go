//go:build windows

package core

import (
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

	controlapi "sdwan-go/pkg/controlapi"
	protocol "sdwan-go/pkg/protocol"
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
	lastAutoAttempt atomic.Int64
	mtuApplying     bool
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
		if tok, err := controlapi.LoadControlToken(m.tokenPath); err == nil {
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
			if err := controlapi.ControlPauseWithTimeout(controlAddr, token, true, 10*time.Second); err != nil {
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

		sr, err := controlapi.ControlStatusWithTimeout(controlAddr, token, 2*time.Second)
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
		if err := controlapi.ControlPauseWithTimeout(controlAddr, token, false, 10*time.Second); err != nil {
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
		sr, err := controlapi.ControlStatusWithTimeout(controlAddr, token, 2*time.Second)
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
			if _, err := controlapi.ControlSwitchWithTimeout(controlAddr, token, targetName, 15*time.Second); err != nil {
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

// OptimizeMTU probes the configured server with Windows ping DF packets,
// updates only the mtu= line in iwan.conf when needed, and restarts the daemon
// by shutdown/start (not /v1/switch) so the new packet size is applied cleanly.
func (m *SdwanManager) OptimizeMTU() map[string]interface{} {
	m.mu.Lock()
	if m.mtuApplying {
		m.mu.Unlock()
		return mtuResult(false, "正在优化 MTU，请稍候。")
	}
	m.mtuApplying = true
	server := m.getCurrentServerName()
	var currentMTU int
	var mtuErr error
	if cfg, err := parseIwanConf(m.iwanPath); err != nil {
		mtuErr = err
	} else {
		currentMTU, mtuErr = strconv.Atoi(cfg["mtu"])
	}
	token := m.token
	controlAddr := m.controlAddr
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		m.mtuApplying = false
		m.mu.Unlock()
	}()

	if server == "" || strings.HasPrefix(server, "节点 ") {
		return mtuResult(false, "未找到当前服务器，未修改 MTU。")
	}
	if mtuErr != nil {
		return mtuResult(false, fmt.Sprintf("读取当前 MTU 失败：%v，未修改。", mtuErr))
	}

	bestPayload, ok := probeMTUPayload(server)
	if !ok {
		return mtuResult(false, fmt.Sprintf("无法探测 %s 的 MTU，未修改。请检查网络或服务器是否允许 ping。", server))
	}

	detectedMTU := clampMTU(bestPayload + 28 - 64)
	if detectedMTU == currentMTU {
		return map[string]interface{}{
			"ok":      true,
			"changed": false,
			"mtu":     detectedMTU,
			"message": fmt.Sprintf("当前 MTU 已是 %d，无需调整。", detectedMTU),
		}
	}

	if err := updateIwanConfKey(m.iwanPath, "mtu", fmt.Sprintf("%d", detectedMTU)); err != nil {
		return mtuResult(false, fmt.Sprintf("写入 MTU 失败：%v，未修改。", err))
	}

	m.mu.Lock()
	m.state = "reconnecting"
	m.connected = false
	m.mu.Unlock()
	if m.onStateChange != nil {
		m.onStateChange()
	}

	if token != "" {
		log.Println("[PANEL] Applying MTU: graceful daemon shutdown")
		if err := controlapi.ControlShutdownWithTimeout(controlAddr, token, 10*time.Second); err != nil {
			log.Printf("[PANEL] MTU shutdown request failed: %v", err)
		}
	}
	released := m.waitDaemonRelease(controlAddr, token)
	if !released {
		return map[string]interface{}{
			"ok":      false,
			"changed": true,
			"mtu":     detectedMTU,
			"message": fmt.Sprintf("MTU 已改为 %d，但旧连接还未释放，请稍后手动重连。", detectedMTU),
		}
	}
	restarted := m.ensureDaemonRunning()
	if !restarted {
		return map[string]interface{}{
			"ok":      false,
			"changed": true,
			"mtu":     detectedMTU,
			"message": fmt.Sprintf("MTU 已改为 %d，但重连未完成，请稍后手动连接。", detectedMTU),
		}
	}
	if m.onStateChange != nil {
		m.onStateChange()
	}
	return map[string]interface{}{
		"ok":      true,
		"changed": true,
		"old_mtu": currentMTU,
		"mtu":     detectedMTU,
		"message": fmt.Sprintf("MTU 已从 %d 优化为 %d，并已重新连接。", currentMTU, detectedMTU),
	}
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

func mtuResult(ok bool, message string) map[string]interface{} {
	return map[string]interface{}{
		"ok":      ok,
		"changed": false,
		"message": message,
	}
}

func clampMTU(mtu int) int {
	if mtu < 1200 {
		return 1200
	}
	if mtu > 1436 {
		return 1436
	}
	return mtu
}

func probeMTUPayload(host string) (int, bool) {
	lo, hi := 548, 1472
	best := 0
	for lo <= hi {
		mid := lo + (hi-lo)/2
		if pingPayload(host, mid) {
			best = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	if best == 0 {
		return 0, false
	}
	return best, true
}

func pingPayload(host string, payload int) bool {
	cmd := hiddenCommand("ping.exe", "-4", "-f", "-l", strconv.Itoa(payload), "-n", "1", "-w", "1000", host)
	return cmd.Run() == nil
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
			err = controlapi.ControlShutdownWithTimeout(controlAddr, token, 10*time.Second)
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

// --- iwan.conf helpers -----------------------------------------------

// parseIwanConf reads all key=value pairs from iwan.conf.
func parseIwanConf(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		result[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	return result, nil
}

// updateIwanConfKey reads iwan.conf, replaces the line matching ^key\s*=,
// and writes it back.
func updateIwanConfKey(path, key, value string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	prefix := key + "="
	found := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, prefix) || strings.HasPrefix(trimmed, key+" ") {
			lines[i] = key + "=" + value
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("key %q not found in %s", key, path)
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644)
}

// --- iwan.conf sync -------------------------------------------------

// syncIwanConf reads the existing iwan.conf and updates the server= line.
// Other fields (username, password, port, mtu, encrypt, etc.) are preserved.
func (m *SdwanManager) syncIwanConf() error {
	serverName := m.getCurrentServerName()

	// Skip if already correct — prevents watcher→reload→sync→watcher loop
	cfg, err := parseIwanConf(m.iwanPath)
	if err != nil {
		return m.writeDefaultIwanConf()
	}
	if cfg["server"] == serverName {
		return nil
	}

	return updateIwanConfKey(m.iwanPath, "server", serverName)
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
		sr, err := controlapi.ControlStatusWithTimeout(controlAddr, token, 2*time.Second)
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
	// But first make sure we have the token; the panel may have been
	// restarted fresh while the daemon kept running.
	if alreadyStarted {
		if token == "" {
			var tokErr error
			m.token, tokErr = controlapi.LoadControlToken(m.tokenPath)
			if tokErr != nil {
				log.Printf("[DAEMON] Token load failed on already-started poll: %v", tokErr)
				return false
			}
			token = m.token
		}
		return m.pollDaemonReady(token, controlAddr)
	}

	// Second check under lock: re-verify no one else started while we were
	// doing the initial API quick-check above.
	m.mu.Lock()
	if m.daemonCmd != nil || m.daemonStarting {
		m.mu.Unlock()
		return m.pollDaemonReady(token, controlAddr)
	}

	// Generate or load the control token BEFORE starting daemon,
	// so both panel and daemon share the same token. Without this
	// pollDaemonReady below gets an empty token and silently fails.
	var tokErr error
	m.token, tokErr = controlapi.LoadControlToken(m.tokenPath)
	if tokErr != nil {
		log.Printf("[DAEMON] Token generation failed: %v", tokErr)
		m.mu.Unlock()
		return false
	}
	token = m.token
	log.Printf("[DAEMON] Token ready, starting daemon at %s", controlAddr)

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
		log.Println("[DAEMON] pollDaemonReady: empty token, cannot poll")
		return false
	}
	log.Printf("[DAEMON] pollDaemonReady: waiting for daemon at %s", controlAddr)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(1 * time.Second)
		if sr, err := controlapi.ControlStatusWithTimeout(controlAddr, token, 2*time.Second); err == nil && sr.State == "running" {
			m.mu.Lock()
			m.state = "running"
			m.connected = true
			m.daemonStarting = false
			m.mu.Unlock()
			log.Println("[DAEMON] Daemon is ready, starting poller")
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
	log.Println("[POLL] Daemon poller started")

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
	sr, err := controlapi.ControlStatusWithTimeout(controlAddr, token, 2*time.Second)

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

	if changed {
		log.Printf("[POLL] State changed: %s → %s", wasState, m.state)
		if m.onStateChange != nil {
			m.onStateChange()
		}
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

// NOTE: The probe protocol functions below (probeConfig, probeLatency,
// loadProbeConfig) use the shared protocol package for packet construction
// and verification instead of duplicating logic.

type probeConfig struct {
	Server string
	Port   int
	Cfg    protocol.OpenConfig
}

// probeLatency sends the SD-WAN OPEN handshake over UDP:10010 and measures
// the time until a valid OPENACK arrives.
func probeLatency(server string) int64 {
	cfg := loadProbeConfig(server)
	if cfg.Cfg.Username == "" || cfg.Cfg.Password == "" {
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

	openPkt := protocol.BuildOpenPacket(cfg.Cfg)
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
		if protocol.MsgType(data) != protocol.MsgOPENACK {
			continue
		}
		if !protocol.PktVerify(data) {
			continue
		}
		ms := time.Since(start).Milliseconds()
		log.Printf("[LATENCY] %s:%d OPEN/OPENACK = %dms", cfg.Server, cfg.Port, ms)
		return ms
	}
}

func loadProbeConfig(server string) probeConfig {
	cfg := probeConfig{
		Server: server,
		Port:   10010,
		Cfg: protocol.OpenConfig{
			MTU:     1436,
			Encrypt: 0,
		},
	}

	parsed, err := parseIwanConf(instance.iwanPath)
	if err != nil {
		return cfg
	}

	if v, ok := parsed["server"]; ok && cfg.Server == "" {
		cfg.Server = v
	}
	if v, ok := parsed["username"]; ok {
		cfg.Cfg.Username = v
	}
	if v, ok := parsed["password"]; ok {
		cfg.Cfg.Password = v
	}
	if v, ok := parsed["port"]; ok {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			cfg.Port = p
		}
	}
	if v, ok := parsed["mtu"]; ok {
		if m, err := strconv.Atoi(v); err == nil && m > 0 {
			cfg.Cfg.MTU = m
		}
	}
	if v, ok := parsed["encrypt"]; ok {
		if e, err := strconv.Atoi(v); err == nil {
			cfg.Cfg.Encrypt = e
		}
	}
	return cfg
}

// --- helpers ---------------------------------------------------------

func (m *SdwanManager) waitDaemonRelease(controlAddr, token string) bool {
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		cmdRunning := m.daemonCmd != nil
		m.mu.Unlock()
		apiReleased := true
		if token != "" {
			if _, err := controlapi.ControlStatusWithTimeout(controlAddr, token, 2*time.Second); err == nil {
				apiReleased = false
			}
		}
		if !cmdRunning && apiReleased {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

func (m *SdwanManager) getCurrentServerName() string {
	for _, s := range m.config.Servers {
		if s.ID == m.config.CurrentServer {
			return s.Name
		}
	}
	return fmt.Sprintf("节点 %s", m.config.CurrentServer)
}
