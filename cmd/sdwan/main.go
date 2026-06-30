package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"strings"

	"sdwan-go/internal/core"
)

// Build metadata — injected via ldflags at build time
var (
	Version   = "dev"
	BuildDate = "unknown"
)

const (
	defaultConfigPath  = "iwan.conf"
	defaultControlAddr = "127.0.0.1:17890"
)

func main() {
	// Subcommands: sdwan status / sdwan switch <server>
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "status":
			runStatusCmd()
			return
		case "switch":
			runSwitchCmd()
			return
		case "-version", "--version":
			fmt.Printf("sdwan %s\n", Version)
			fmt.Printf("  Build: %s\n", BuildDate)
			fmt.Printf("  Language: Go %s\n", runtime.Version())
			fmt.Printf("  Platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
			return
		}
	}

	// Legacy mode: -f, -daemon, -version flags via flag.Parse
	configPath := flag.String("f", defaultConfigPath, "config file path")
	daemon := flag.Bool("daemon", false, "run as daemon (non-blocking, future control API)")
	controlAddr := flag.String("control", defaultControlAddr, "control API listen address (daemon mode only)")
	tokenFile := flag.String("token-file", "", "path to static token file (daemon mode only)")
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

	if *daemon {
		opts := core.ControlOptions{
			Addr:      *controlAddr,
			TokenFile: *tokenFile,
		}
		if err := core.RunDaemon(*configPath, opts); err != nil {
			log.Fatalf("[FATAL] %v", err)
		}
		return
	}

	// Delegate to the reusable one-shot orchestration.
	// RunOnce takes over config loading, UDP connect, handshake,
	// TUN creation, route setup, main loop, signal handling, and cleanup.
	if err := core.RunOnce(*configPath); err != nil {
		log.Fatalf("[FATAL] %v", err)
	}
}

func resolveToken(configPath, tokenFile string) (string, error) {
	if tokenFile != "" {
		return core.LoadControlToken(tokenFile)
	}
	return core.LoadControlToken(core.DefaultTokenPath(configPath))
}

// ---------- status subcommand ----------

func runStatusCmd() {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	configPath := fs.String("f", defaultConfigPath, "config file path (for token location)")
	controlAddr := fs.String("control", defaultControlAddr, "control API address")
	tokenFile := fs.String("token-file", "", "path to token file")
	asJSON := fs.Bool("json", false, "output raw JSON")
	_ = fs.Parse(os.Args[2:]) // nolint

	token, err := resolveToken(*configPath, *tokenFile)
	if err != nil {
		log.Fatalf("[FATAL] token: %v", err)
	}

	sr, err := core.ControlStatus(*controlAddr, token)
	if err != nil {
		log.Fatalf("[FATAL] status: %v", err)
	}

	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(sr)
		return
	}

	fmt.Printf("State:     %s\n", sr.State)
	fmt.Printf("Server:    %s:%d\n", sr.Server, sr.Port)
	fmt.Printf("Session:   %d\n", sr.SessionID)
	fmt.Printf("TUN:       %s\n", sr.TUN)
	fmt.Printf("Local IP:  %s\n", sr.LocalIP)
	fmt.Printf("Gateway:   %s\n", sr.GatewayIP)
	fmt.Printf("Route:     %s\n", sr.Route)
	fmt.Printf("MTU:       %d\n", sr.MTU)
}

// ---------- switch subcommand ----------

func runSwitchCmd() {
	fs := flag.NewFlagSet("switch", flag.ExitOnError)
	configPath := fs.String("f", defaultConfigPath, "config file path (for token location)")
	controlAddr := fs.String("control", defaultControlAddr, "control API address")
	tokenFile := fs.String("token-file", "", "path to token file")
	asJSON := fs.Bool("json", false, "output raw JSON")

	server := ""
	argsForFlags := os.Args[2:]
	if len(argsForFlags) > 0 && !strings.HasPrefix(argsForFlags[0], "-") {
		server = argsForFlags[0]
		argsForFlags = argsForFlags[1:]
	}
	_ = fs.Parse(argsForFlags) // nolint

	if server == "" {
		args := fs.Args()
		if len(args) > 0 {
			server = args[0]
		}
	}
	server = strings.TrimSpace(server)
	if server == "" {
		log.Fatalf("[FATAL] usage: sdwan switch <server> [flags]")
	}

	token, err := resolveToken(*configPath, *tokenFile)
	if err != nil {
		log.Fatalf("[FATAL] token: %v", err)
	}

	resp, err := core.ControlSwitch(*controlAddr, token, server)
	if err != nil {
		log.Fatalf("[FATAL] switch: %v", err)
	}

	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(resp)
		return
	}
	if resp.Status == nil {
		log.Fatalf("[FATAL] malformed switch response: missing status")
	}

	fmt.Printf("Switched to: %s\n", server)
	fmt.Printf("Session:     %d\n", resp.Status.SessionID)
	if resp.Tunnel != nil {
		fmt.Printf("Local IP:    %s\n", resp.Tunnel.LocalIP)
	}
	fmt.Printf("TUN:         %s\n", resp.Status.TUN)
}
