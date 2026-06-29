package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"sdwan-go/internal/core"
)

// Build metadata — injected via ldflags at build time
var (
	Version   = "dev"
	BuildDate = "unknown"
)

const (
	defaultConfigPath = "iwan.conf"
)

func main() {
	configPath := flag.String("f", defaultConfigPath, "config file path")
	showVersion := flag.Bool("version", false, "show version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("sdwan %s\n", Version)
		fmt.Printf("  Build: %s\n", BuildDate)
		fmt.Printf("  Language: Go %s\n", runtime.Version())
		fmt.Printf("  Platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
		return
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	// Write log to sdwan.log (in addition to stderr) so errors are
	// captured even if the console window closes immediately on Windows.
	logFile, err := os.Create("sdwan.log")
	if err == nil {
		defer logFile.Close()
		log.SetOutput(io.MultiWriter(os.Stderr, logFile))
	}

	log.Printf("[INFO] SDWAN Go client starting, config=%s", *configPath)

	// 1. Load config
	cfg, err := core.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("[FATAL] Config error: %v", err)
	}

	log.Printf("[INFO] Server=%s Port=%d User=%s MTU=%d Encrypt=%d",
		cfg.Server, cfg.Port, cfg.Username, cfg.MTU, cfg.Encrypt)

	// 2. Create client
	client, err := core.NewClient(cfg)
	if err != nil {
		log.Fatalf("[FATAL] Create client: %v", err)
	}
	defer client.Close()

	// 3. Connect to server
	if err := client.Connect(); err != nil {
		log.Fatalf("[FATAL] UDP connect: %v", err)
	}
	log.Printf("[INFO] UDP connected to %s:%d", cfg.Server, cfg.Port)

	// 4. Handshake
	log.Println("[AUTH] Waiting for OPENACK...")
	openAck, err := client.Handshake()
	if err != nil {
		log.Fatalf("[FATAL] Handshake: %v", err)
	}
	log.Println("[AUTH] Authenticated successfully")

	// Parse TUN configuration from OPENACK
	tunCfg := core.ParseOPENACK(openAck)
	if tunCfg.LocalIP == "" || tunCfg.GatewayIP == "" {
		log.Fatalf("[FATAL] OPENACK missing IP info: local=%q gateway=%q", tunCfg.LocalIP, tunCfg.GatewayIP)
	}
	log.Printf("[TUN] Local IP=%s Gateway=%s DNS=%s MTU=%d",
		tunCfg.LocalIP, tunCfg.GatewayIP, tunCfg.DNSIP, tunCfg.MTU)

	// Override config MTU if server sent one
	if tunCfg.MTU > 0 {
		cfg.MTU = int(tunCfg.MTU)
	}

	// 5. Create TUN (Windows TUN mode needs local CIDR to configure the driver)
	localCIDR := tunCfg.LocalIP + "/24"
	tun, err := core.CreateTUN(cfg.TUNName, cfg.MTU, localCIDR)
	if err != nil {
		log.Fatalf("[FATAL] Create TUN: %v", err)
	}
	client.TUN = tun
	defer core.CloseTUN(tun, cfg.TUNName)
	log.Printf("[TUN] Created %s (MTU=%d)", tun.Name(), cfg.MTU)

	// 6. Assign IP and bring up
	tunName := tun.Name()
	if err := core.SetTUNIP(tunName, localCIDR, tunCfg.GatewayIP); err != nil {
		log.Printf("[WARN] Set TUN IP failed: %v", err)
	} else {
		log.Printf("[TUN] %s IP=%s/24 gateway=%s", tunName, tunCfg.LocalIP, tunCfg.GatewayIP)
	}

	// 7. Add route
	routeGW := tunCfg.LocalIP
	if err := core.AddRoute(cfg.RouteNet, tunName, routeGW); err != nil {
		log.Printf("[WARN] Route add failed (may need to wait): %v", err)
		// Retry after delay
		time.Sleep(3 * time.Second)
		if err := core.AddRoute(cfg.RouteNet, tunName, routeGW); err != nil {
			log.Printf("[WARN] Route still failed: %v", err)
		}
	}
	defer core.DelRoute(cfg.RouteNet, tunName, routeGW)
	log.Printf("[ROUTE] Added %s -> %s", cfg.RouteNet, cfg.TUNName)

	// 8. Handle signals for clean shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// 9. Show status
	showStatus := func() {
		fmt.Println()
		log.Println("[STATUS] SDWAN tunnel is running")
		log.Printf("  Server:  %s:%d", cfg.Server, cfg.Port)
		log.Printf("  User:    %s", cfg.Username)
		log.Printf("  Session: %d", client.SessionID)
		log.Printf("  TUN:     %s", tunName)
		log.Printf("  Route:   %s -> %s", cfg.RouteNet, tunName)
		fmt.Println()
	}
	showStatus()

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
}
