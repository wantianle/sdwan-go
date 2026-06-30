package core

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
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
	client, err := NewClient(cfg)
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}
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
	tunCfg := ParseOPENACK(openAck)
	if tunCfg.LocalIP == "" || tunCfg.GatewayIP == "" {
		return fmt.Errorf("OPENACK missing IP info: local=%q gateway=%q",
			tunCfg.LocalIP, tunCfg.GatewayIP)
	}
	log.Printf("[TUN] Local IP=%s Gateway=%s DNS=%s MTU=%d",
		tunCfg.LocalIP, tunCfg.GatewayIP, tunCfg.DNSIP, tunCfg.MTU)

	// Override config MTU if server sent one
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
	log.Printf("  Session: %d", client.SessionID)
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

// setupTUN creates the TUN adapter, assigns the server-assigned IP,
// and adds the route with retry. Returns the adapter name and a cleanup
// function that caller must defer (DelRoute + CloseTUN).
func setupTUN(cfg *Config, tunCfg *OPENACKResult, client *Client) (tunName string, cleanup func(), err error) {
	localCIDR := tunCfg.LocalIP + "/24"
	tun, err := CreateTUN(cfg.TUNName, cfg.MTU, localCIDR)
	if err != nil {
		return "", nil, fmt.Errorf("create TUN: %w", err)
	}
	client.TUN = tun
	log.Printf("[TUN] Created %s (MTU=%d)", tun.Name(), cfg.MTU)

	tunName = tun.Name()
	if err := SetTUNIP(tunName, localCIDR, tunCfg.GatewayIP); err != nil {
		log.Printf("[WARN] Set TUN IP failed: %v", err)
	} else {
		log.Printf("[TUN] %s IP=%s/24 gateway=%s", tunName, tunCfg.LocalIP, tunCfg.GatewayIP)
	}

	routeGW := tunCfg.LocalIP
	if err := AddRoute(cfg.RouteNet, tunName, routeGW); err != nil {
		log.Printf("[WARN] Route add failed (may need to wait): %v", err)
		// Retry after delay
		time.Sleep(3 * time.Second)
		if err := AddRoute(cfg.RouteNet, tunName, routeGW); err != nil {
			log.Printf("[WARN] Route still failed: %v", err)
		}
	}
	log.Printf("[ROUTE] Added %s -> %s", cfg.RouteNet, cfg.TUNName)

	cleanup = func() {
		DelRoute(cfg.RouteNet, tunName, routeGW)
		CloseTUN(tun, cfg.TUNName)
	}
	return tunName, cleanup, nil
}
