package core

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	controlapi "sdwan-go/pkg/controlapi"
	protocol "sdwan-go/pkg/protocol"
)

// RunOnce loads iwan.conf from configPath and runs the full one-shot SD-WAN
// tunnel lifecycle: load config, connect UDP, handshake, create TUN, assign
// IP, add route, then block in the main loop until a signal or error.
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

	// 3. Connect to server
	if err := client.Connect(); err != nil {
		return fmt.Errorf("UDP connect: %w", err)
	}
	log.Printf("[INFO] UDP connected to %s:%d", cfg.Server, cfg.Port)

	// 4. Handshake
	log.Println("[AUTH] Waiting for OPENACK...")
	openAck, err := client.Handshake()
	if err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	log.Println("[AUTH] Authenticated successfully")

	// Parse TUN configuration from OPENACK
	tunCfg := protocol.ParseOPENACK(openAck)
	if tunCfg.LocalIP == "" || tunCfg.GatewayIP == "" {
		return fmt.Errorf("OPENACK missing IP info: local=%q gateway=%q",
			tunCfg.LocalIP, tunCfg.GatewayIP)
	}
	log.Printf("[TUN] Local IP=%s Gateway=%s DNS=%s MTU=%d",
		tunCfg.LocalIP, tunCfg.GatewayIP, tunCfg.DNSIP, tunCfg.MTU)

	// 4. Apply server-assigned tunnel configuration
	client.SetTunnelConfig(tunCfg)
	if tunCfg.MTU > 0 {
		cfg.MTU = int(tunCfg.MTU)
	}

	// 5. Create TUN, assign IP, add route (with retry + cleanup wiring)
	tunName, tunCleanup, err := setupTUN(cfg, tunCfg, client)
	if err != nil {
		return err
	}
	defer tunCleanup()

	// 8. Handle signals for clean shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// 9. Show status
	fmt.Println()
	log.Println("[STATUS] SDWAN tunnel is running")
	log.Printf("  Server:  %s:%d", cfg.Server, cfg.Port)
	log.Printf("  User:    %s", cfg.Username)
	log.Printf("  Session: %d", client.SessionID())
	log.Printf("  TUN:     %s", tunName)
	log.Printf("  Route:   %s -> %s", cfg.RouteNet, tunName)
	fmt.Println()

	// 10. Run main loop in background
	errCh := make(chan error, 1)
	go func() {
		errCh <- client.Run()
	}()

	// 11. Wait for signal or error
	select {
	case sig := <-sigCh:
		log.Printf("[INFO] Received signal %v, shutting down...", sig)
	case err := <-errCh:
		if err != nil {
			log.Printf("[ERROR] Client error: %v", err)
		}
	}

	log.Println("[INFO] Shutdown complete")
	return nil
}

// ControlOptions holds daemon-mode local control API settings.
type ControlOptions struct {
	Addr      string // control listen address, e.g. "127.0.0.1:17890"
	TokenFile string // optional path to a static token file
}

// tryStartup performs the full initial connect/handshake/TUN/Start sequence
// with retry. Returns nil on success. Auth rejection stops retrying immediately.
// Network, DNS, and other transient errors trigger backoff and retry.
func tryStartup(client *Client, cfg *Config) {
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

		client.SetTunnelConfig(tunCfg)
		if tunCfg.MTU > 0 {
			cfg.MTU = int(tunCfg.MTU)
		}

		// 4. Setup TUN
		tunName, tunCleanup, err := setupTUN(cfg, tunCfg, client)
		if err != nil {
			log.Printf("[STARTUP] TUN setup failed: %v", err)
			client.TUN = nil // clear TUN on failure
			// setupTUN already cleans up on failure
			backoff = sleepWithBackoff(client, backoff)
			continue
		}
		// Store cleanup so RunDaemon's deferred Close() tears down TUN + routes
		client.SetTUNCleanup(tunCleanup)

		log.Printf("[ROUTE] Added %s -> %s", cfg.RouteNet, tunName)

		// 5. Start daemon loops
		if err := client.Start(); err != nil {
			log.Printf("[STARTUP] Daemon start failed: %v", err)
			tunCleanup()     // tear down TUN/routes we just created
			client.TUN = nil // clear TUN reference
			backoff = sleepWithBackoff(client, backoff)
			continue
		}

		log.Printf("[INFO] Tunnel established, starting daemon loops...")

		client.setReady()

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
// client.Run(). It then waits for SIGINT/SIGTERM, cleans up, and returns.
//
// The control API server is not implemented yet; the daemon simply stays
// alive so future control clients can attach once the HTTP server is added.
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

	// 5. Start tunnel in background goroutine
	go tryStartup(client, cfg)

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
	log.Println("[INFO] Daemon shutdown complete")
	return nil
}

// setupTUN creates the TUN adapter, assigns the server-assigned IP,
// and adds the route with retry. Returns the adapter name and a cleanup
// function that caller must defer (DelRoute + CloseTUN).
func setupTUN(cfg *Config, tunCfg *protocol.OPENACKResult, client *Client) (tunName string, cleanup func(), err error) {
	localCIDR := tunCfg.LocalIP + "/24"
	tun, err := CreateTUN(cfg.TUNName, cfg.MTU, localCIDR)
	if err != nil {
		return "", nil, fmt.Errorf("create TUN: %w", err)
	}
	client.TUN = tun
	log.Printf("[TUN] Created %s (MTU=%d)", tun.Name(), cfg.MTU)

	tunName = tun.Name()
	if err := SetTUNIP(tunName, localCIDR, tunCfg.GatewayIP); err != nil {
		CloseTUN(tun, cfg.TUNName)
		return "", nil, fmt.Errorf("set TUN IP: %w", err)
	}
	log.Printf("[TUN] %s IP=%s/24 gateway=%s", tunName, tunCfg.LocalIP, tunCfg.GatewayIP)

	// Check for route conflicts between SDWAN routenet and local LAN subnets
	conflicts := detectRouteConflicts(cfg.RouteNet, tunName)
	if len(conflicts) > 0 {
		client.SetRouteConflicts(conflicts)
		for _, c := range conflicts {
			log.Printf("[ROUTE] WARNING: SDWAN route %s overlaps with local %s subnet %s — local traffic for %s will NOT go through VPN",
				c.RouteNet, c.Interface, c.LocalCIDR, c.LocalCIDR)
		}
	}

	routeGW := tunCfg.LocalIP
	if err := AddRoute(cfg.RouteNet, tunName, routeGW); err != nil {
		log.Printf("[WARN] Route add failed (may need to wait): %v", err)
		time.Sleep(3 * time.Second)
		if err := AddRoute(cfg.RouteNet, tunName, routeGW); err != nil {
			DelRoute(cfg.RouteNet, tunName, routeGW)
			CloseTUN(tun, cfg.TUNName)
			return "", nil, fmt.Errorf("add route: %w", err)
		}
	}
	log.Printf("[ROUTE] Added %s -> %s", cfg.RouteNet, tunName)

	cleanup = func() {
		DelRoute(cfg.RouteNet, tunName, routeGW)
		CloseTUN(tun, cfg.TUNName)
	}
	return tunName, cleanup, nil
}
