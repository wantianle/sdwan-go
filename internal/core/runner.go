package core

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	controlapi "sdwan-go/pkg/controlapi"
	protocol "sdwan-go/pkg/protocol"
)

// protocolIPv4Prefix is the protocol-compatibility /24 until the protocol
// carries a prefix explicitly.
const protocolIPv4Prefix = 24

// Bounded retry policy for RunOnce's initial UDP connect + handshake. A short
// transient startup network hiccup must not force systemd/launchd to restart
// the whole tunnel (which would reassign a new tunnel IP).
const (
	runOnceStartupAttempts = 3
	runOnceStartupBackoff  = 500 * time.Millisecond
)

// Test seams for RunOnce. Production uses the real implementations; focused
// tests override these to avoid requiring CAP_NET_ADMIN (real TUN device) and
// to drive shutdown without delivering process-wide signals.
var (
	runOnceSetupTUN = setupTUN
	runOnceSignals  = func() (<-chan os.Signal, func()) {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		return ch, func() { signal.Stop(ch) }
	}
)

// RunOnce loads iwan.conf from configPath and runs the full foreground SD-WAN
// tunnel lifecycle: load config, connect UDP (bounded startup retry),
// handshake, create TUN, assign IP, add route, start the self-healing session
// loops, then block until a signal. Transient session failures are recovered
// in-process by the reconnect worker instead of returning and exiting, so a
// link blip does not trigger a supervisor restart with a new tunnel IP.
//
// Callers own log-file setup and CLI argument parsing; RunOnce uses the
// global log package so any log output configured by the caller is preserved.
func RunOnce(configPath string) error {
	// 1. Load config
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	log.Printf("[INFO] Server=%s Port=%d User=%s MTU=%d Encrypt=%d",
		cfg.Server, cfg.Port, cfg.Username, cfg.MTU, cfg.Encrypt)

	// 2. Create client
	client := NewClient(cfg)
	defer client.Close()

	// 3+4. Connect and handshake with a bounded startup retry so a transient
	// startup-time network hiccup does not force a systemd/launchd restart.
	// Auth rejection is terminal and is never retried.
	log.Println("[AUTH] Waiting for OPENACK...")
	openAck, err := connectWithRetry(client.Connect, client.Handshake,
		runOnceStartupAttempts, runOnceStartupBackoff)
	if err != nil {
		return err
	}
	log.Printf("[INFO] UDP connected to %s:%d", cfg.Server, cfg.Port)
	log.Println("[AUTH] Authenticated successfully")

	// Parse TUN configuration from OPENACK
	tunCfg := protocol.ParseOPENACK(openAck)
	if tunCfg.LocalIP == "" || tunCfg.GatewayIP == "" {
		return fmt.Errorf("OPENACK missing IP info: local=%q gateway=%q",
			tunCfg.LocalIP, tunCfg.GatewayIP)
	}
	log.Printf("[TUN] Local IP=%s Gateway=%s DNS=%s MTU=%d",
		tunCfg.LocalIP, tunCfg.GatewayIP, tunCfg.DNSIP, tunCfg.MTU)

	// 5. Apply server-assigned tunnel configuration
	client.SetTunnelConfig(tunCfg)
	if tunCfg.MTU > 0 {
		cfg.MTU = int(tunCfg.MTU)
		client.setConfigMTU(cfg.MTU)
	}

	// 6. Create TUN, assign IP, add route (with retry + cleanup wiring)
	tunName, tunCleanup, err := runOnceSetupTUN(cfg, tunCfg, client)
	if err != nil {
		return err
	}
	defer tunCleanup()

	// 7. Handle signals for clean shutdown
	sigCh, stopSignals := runOnceSignals()
	defer stopSignals()

	// 8. Show status
	fmt.Println()
	log.Println("[STATUS] SDWAN tunnel is running")
	log.Printf("  Server:  %s:%d", cfg.Server, cfg.Port)
	log.Printf("  User:    %s", cfg.Username)
	log.Printf("  Session: %d", client.SessionID())
	log.Printf("  TUN:     %s", tunName)
	log.Printf("  Route:   %s -> %s", cfg.RouteNet, tunName)
	fmt.Println()

	// 9. Start the self-healing session loops (session loops + packet pump +
	// reconnect worker) and block until a shutdown signal. Transient session
	// failures are recovered in-process instead of returning and exiting.
	sig, err := runTunnelPhase(client, sigCh)
	if err != nil {
		return err
	}
	log.Printf("[INFO] Received signal %v, shutting down...", sig)

	// 10. Close before the deferred route/TUN cleanup so the session loops and
	// packet pump stop while the adapter is still alive.
	client.Close()

	log.Println("[INFO] Shutdown complete")
	return nil
}

// connectWithRetry performs the initial UDP connect + handshake with a bounded
// number of attempts and a short fixed backoff between them. Resolve, dial and
// read-timeout errors are transient and retried; authentication rejection is
// terminal and returned immediately without another attempt.
func connectWithRetry(connect func() error, handshake func() ([]byte, error), attempts int, backoff time.Duration) ([]byte, error) {
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := connect(); err != nil {
			lastErr = fmt.Errorf("UDP connect: %w", err)
		} else {
			raw, err := handshake()
			if err == nil {
				return raw, nil
			}
			lastErr = fmt.Errorf("handshake: %w", err)
		}
		if isAuthRejection(lastErr) {
			return nil, lastErr
		}
		if attempt < attempts {
			log.Printf("[STARTUP] Initial connect attempt %d/%d failed: %v; retrying in %s",
				attempt, attempts, lastErr, backoff)
			time.Sleep(backoff)
		}
	}
	return nil, lastErr
}

// runTunnelPhase starts the client's self-healing session loops via Start
// (heartbeat + server→TUN + TUN→server packet pump + reconnect worker,
// preserving the first-ECHOREQ-before-DATA ordering) and blocks until a
// shutdown signal is delivered on sigCh. Transient session failures are
// absorbed by the reconnect worker and never return control here, so the
// foreground process stays alive instead of letting the service supervisor
// restart the tunnel with a new IP.
func runTunnelPhase(client *Client, sigCh <-chan os.Signal) (os.Signal, error) {
	if err := client.Start(); err != nil {
		return nil, fmt.Errorf("start client: %w", err)
	}
	log.Println("[INFO] Auto-reconnect enabled: transient session failures will be recovered in-process")
	sig := <-sigCh
	return sig, nil
}

// ControlOptions holds daemon-mode local control API settings.
type ControlOptions struct {
	Addr      string // control listen address, e.g. "127.0.0.1:17890"
	TokenFile string // optional path to a static token file
}

// tryStartup starts and joins one initial daemon lifecycle attempt. Production
// starts it through startTryStartup so admission occurs before the goroutine is
// launched; this wrapper keeps direct callers safe as well.
func tryStartup(client *Client, cfg *Config) {
	if !client.beginLifecycle() {
		return
	}
	defer client.endLifecycle()
	tryStartupAdmitted(client, cfg)
}

func startTryStartup(client *Client, cfg *Config) bool {
	if !client.beginLifecycle() {
		return false
	}
	go func() {
		defer client.endLifecycle()
		tryStartupAdmitted(client, cfg)
	}()
	return true
}

// tryStartupAdmitted performs the full initial connect/handshake/TUN/Start
// sequence with retry. The caller must have already admitted lifecycle work.
func tryStartupAdmitted(client *Client, cfg *Config) {
	cfg = cloneConfig(cfg)
	if cfg == nil {
		return
	}
	client.startupPending.Store(true)
	defer client.startupPending.Store(false)

	backoff := 500 * time.Millisecond
	for {
		if client.isStopped() {
			log.Println("[STARTUP] Client stopped, aborting startup")
			return
		}

		log.Printf("[STARTUP] Attempting initial connect to %s:%d", cfg.Server, cfg.Port)

		// 1. Connect UDP
		if err := client.Connect(); err != nil {
			log.Printf("[STARTUP] UDP connect failed: %v", err)
			if isAuthRejection(err) {
				log.Printf("[FATAL] Startup: auth rejected (not retrying)")
				return
			}
			backoff = sleepWithBackoff(client, backoff)
			continue
		}
		log.Printf("[INFO] UDP connected to %s:%d", cfg.Server, cfg.Port)

		// 2. Handshake
		log.Println("[AUTH] Waiting for OPENACK...")
		openAck, err := client.Handshake()
		if err != nil {
			log.Printf("[STARTUP] Handshake failed: %v", err)
			if isAuthRejection(err) {
				log.Printf("[FATAL] Startup: auth rejected (not retrying)")
				return
			}
			backoff = sleepWithBackoff(client, backoff)
			continue
		}
		log.Println("[AUTH] Authenticated successfully")

		// 3. Parse OPENACK
		tunCfg := protocol.ParseOPENACK(openAck)
		if tunCfg.LocalIP == "" || tunCfg.GatewayIP == "" {
			log.Printf("[STARTUP] OPENACK missing IP info: local=%q gateway=%q", tunCfg.LocalIP, tunCfg.GatewayIP)
			backoff = sleepWithBackoff(client, backoff)
			continue
		}
		log.Printf("[TUN] Local IP=%s Gateway=%s DNS=%s MTU=%d", tunCfg.LocalIP, tunCfg.GatewayIP, tunCfg.DNSIP, tunCfg.MTU)

		if !client.SetTunnelConfig(tunCfg) {
			return
		}
		if tunCfg.MTU > 0 {
			cfg.MTU = int(tunCfg.MTU)
			client.setConfigMTU(cfg.MTU)
		}

		// 4. Setup TUN
		tunName, tunCleanup, err := setupTUN(cfg, tunCfg, client)
		if err != nil {
			log.Printf("[STARTUP] TUN setup failed: %v", err)
			client.clearTUN(nil) // clear TUN on failure
			// setupTUN already cleans up on failure
			backoff = sleepWithBackoff(client, backoff)
			continue
		}
		// Store cleanup so RunDaemon's deferred Close() tears down TUN + routes
		client.SetTUNCleanup(tunCleanup)
		if client.isStopped() {
			return
		}

		log.Printf("[ROUTE] Added %s -> %s", cfg.RouteNet, tunName)

		// 5. Start daemon loops
		if err := client.Start(); err != nil {
			log.Printf("[STARTUP] Daemon start failed: %v", err)
			client.runTUNCleanup()
			client.clearTUN(nil) // clear TUN reference
			backoff = sleepWithBackoff(client, backoff)
			continue
		}

		log.Printf("[INFO] Tunnel established, starting daemon loops...")

		// Final ready publication is atomic with the stopped-state check.
		if !client.publishStartupReady() {
			return
		}

		// Trigger the existing reconnect mechanism so future disconnects are handled
		select {
		case client.reconnectCh <- struct{}{}:
		default:
		}

		// Show status
		fmt.Println()
		log.Println("[STATUS] SDWAN daemon running")
		log.Printf("  Server:  %s:%d", cfg.Server, cfg.Port)
		log.Printf("  User:    %s", cfg.Username)
		log.Printf("  Session: %d", client.SessionID())
		log.Printf("  TUN:     %s", tunName)
		log.Printf("  Route:   %s -> %s", cfg.RouteNet, tunName)
		fmt.Println()

		// Success — stay in this state, reconnectLoop handles future failures
		return
	}
}

func isAuthRejection(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "AUTH REJECTED") ||
		strings.Contains(msg, "auth rejected") ||
		strings.Contains(msg, "authentication failed")
}

func sleepWithBackoff(client *Client, backoff time.Duration) time.Duration {
	select {
	case <-client.stopCh:
		return backoff
	case <-time.After(backoff):
	}
	if backoff < 8*time.Second {
		backoff *= 2
		if backoff > 8*time.Second {
			backoff = 8 * time.Second
		}
	}
	return backoff
}

// RunDaemon performs the same initial setup as RunOnce (config, UDP connect,
// handshake, TUN, routes) but calls client.Start() instead of blocking on
// client.Run(). It starts the localhost control API, then waits for
// SIGINT/SIGTERM or an API shutdown request, cleans up, and returns.
func RunDaemon(configPath string, opts ControlOptions) error {
	// 1. Load config
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	log.Printf("[INFO] Server=%s Port=%d User=%s MTU=%d Encrypt=%d",
		cfg.Server, cfg.Port, cfg.Username, cfg.MTU, cfg.Encrypt)

	// 2. Create client
	client := NewClient(cfg)
	defer client.Close()

	// 3. Load or generate control token
	tokenFile := opts.TokenFile
	if tokenFile == "" {
		tokenFile = DefaultTokenPath(configPath)
	}
	token, err := controlapi.LoadOrCreateControlToken(tokenFile)
	if err != nil {
		return fmt.Errorf("control token: %w", err)
	}

	// 4. Start control API immediately (before tunnel is ready)
	shutdownCh := make(chan struct{}, 1)
	srv, err := startControlServer(opts.Addr, token, client, shutdownCh)
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	log.Printf("[CTRL] Control API listening on %s (awaiting tunnel...)", opts.Addr)

	// 5. Admit startup before launching it so Close always joins its completion.
	if !startTryStartup(client, cfg) {
		return fmt.Errorf("daemon is stopping")
	}

	// 6. Signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// 7. Show initial status
	fmt.Println()
	log.Println("[STATUS] SDWAN daemon starting")
	log.Printf("  Control: %s  (token: %s)", opts.Addr, tokenFile)
	log.Printf("  Target:  %s:%d", cfg.Server, cfg.Port)
	log.Printf("  User:    %s", cfg.Username)
	log.Println("  Status:  reconnecting...")
	fmt.Println()

	// 8. Wait for shutdown signal (SIGINT/SIGTERM or API shutdown)
	select {
	case sig := <-sigCh:
		log.Printf("[INFO] Received signal %v, shutting down...", sig)
	case <-shutdownCh:
		log.Println("[INFO] Received shutdown via control API")
	}
	// Close admission and join startup/switch/reconnect before route/TUN cleanup.
	client.Close()
	log.Println("[INFO] Daemon shutdown complete")
	return nil
}

type tunSetupOps struct {
	create    func(string, int, string) (TunDevice, error)
	setIP     func(string, string, string) error
	addRoute  func(string, string, string) error
	delRoute  func(string, string, string)
	close     func(TunDevice, string)
	conflicts func(string, string) []RouteConflict
}

// setupTUN creates and configures a TUN adapter, then transfers ownership of
// the successfully added route and exact adapter through one idempotent cleanup.
func setupTUN(cfg *Config, tunCfg *protocol.OPENACKResult, client *Client) (tunName string, cleanup func(), err error) {
	return setupTUNWithOps(cfg, tunCfg, client, tunSetupOps{
		create: func(name string, mtu int, cidr string) (TunDevice, error) {
			return CreateTUN(name, mtu, cidr)
		},
		setIP: SetTUNIP, addRoute: AddRoute, delRoute: DelRoute, close: CloseTUN, conflicts: detectRouteConflicts,
	})
}

func setupTUNWithOps(cfg *Config, tunCfg *protocol.OPENACKResult, client *Client, ops tunSetupOps) (tunName string, cleanup func(), err error) {
	localCIDR, err := tunnelLocalCIDR(tunCfg.LocalIP)
	if err != nil {
		return "", nil, err
	}
	if err := validateTunnelGateway(tunCfg.GatewayIP); err != nil {
		return "", nil, err
	}
	tun, err := ops.create(cfg.TUNName, cfg.MTU, localCIDR)
	if err != nil {
		return "", nil, fmt.Errorf("create TUN: %w", err)
	}
	log.Printf("[TUN] Created %s (MTU=%d)", tun.Name(), cfg.MTU)

	tunName = tun.Name()
	if err := ops.setIP(tunName, localCIDR, tunCfg.GatewayIP); err != nil {
		ops.close(tun, tunName)
		return "", nil, fmt.Errorf("set TUN IP: %w", err)
	}
	log.Printf("[TUN] %s IP=%s/24 gateway=%s", tunName, tunCfg.LocalIP, tunCfg.GatewayIP)

	var conflicts []RouteConflict
	if ops.conflicts != nil {
		conflicts = ops.conflicts(cfg.RouteNet, tunName)
	}

	routeGW := tunCfg.LocalIP
	if err := ops.addRoute(cfg.RouteNet, tunName, routeGW); err != nil {
		ops.close(tun, tunName)
		return "", nil, fmt.Errorf("add route: %w", err)
	}
	log.Printf("[ROUTE] Added %s -> %s", cfg.RouteNet, tunName)

	// Commit only after the adapter and route are both fully configured.
	if !client.publishTUN(tun) {
		ops.delRoute(cfg.RouteNet, tunName, routeGW)
		ops.close(tun, tunName)
		return "", nil, fmt.Errorf("%w", errDaemonStopping)
	}
	if !client.SetRouteConflicts(conflicts) {
		ops.delRoute(cfg.RouteNet, tunName, routeGW)
		client.clearTUN(tun)
		ops.close(tun, tunName)
		return "", nil, fmt.Errorf("%w", errDaemonStopping)
	}
	for _, c := range conflicts {
		log.Printf("[ROUTE] WARNING: SDWAN route %s overlaps with local %s subnet %s — local traffic for %s will NOT go through VPN",
			c.RouteNet, c.Interface, c.LocalCIDR, c.LocalCIDR)
	}

	var cleanupOnce sync.Once
	cleanup = func() {
		cleanupOnce.Do(func() {
			// Route ownership is released before the handle, as required by A2.
			ops.delRoute(cfg.RouteNet, tunName, routeGW)
			client.clearTUN(tun)
			ops.close(tun, tunName)
		})
	}
	return tunName, cleanup, nil
}

func tunnelLocalCIDR(local string) (string, error) {
	addr, err := netip.ParseAddr(local)
	if err != nil || !addr.Is4() || addr.IsUnspecified() {
		return "", fmt.Errorf("invalid local IPv4 address: %s", local)
	}
	return netip.PrefixFrom(addr, protocolIPv4Prefix).String(), nil
}

func validateTunnelGateway(gateway string) error {
	addr, err := netip.ParseAddr(gateway)
	if err != nil || !addr.Is4() || addr.IsUnspecified() {
		return fmt.Errorf("invalid gateway IPv4 address: %s", gateway)
	}
	return nil
}
