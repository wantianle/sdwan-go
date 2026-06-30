package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"

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

	// Delegate to the reusable one-shot orchestration.
	// RunOnce takes over config loading, UDP connect, handshake,
	// TUN creation, route setup, main loop, signal handling, and cleanup.
	if err := core.RunOnce(*configPath); err != nil {
		log.Fatalf("[FATAL] %v", err)
	}
}
