package main

import (
	"context"
	"embed"
	"log"
	"os"
	"runtime/debug"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed all:frontend
var assets embed.FS

// appCtx holds the Wails runtime context for window operations.
var appCtx context.Context

func main() {
	// Crash recovery — log panics instead of silently exiting
	defer func() {
		if r := recover(); r != nil {
			log.Printf("FATAL PANIC: %v\n%s", r, debug.Stack())
			os.Exit(1)
		}
	}()

	app := NewApp()

	if f, err := os.Create("sdwan-panel.log"); err == nil {
		log.SetOutput(f)
		defer f.Close()
	}
	log.Println("SDWAN Panel starting...")
	log.Println("[DEBUG] Panel version 1.0.0")

	go safeSystray()

	// Show-panel signal watcher — positions panel at bottom-right
	go func() {
		for range trayShowCh {
			log.Println("[DEBUG] Show panel signal received")
			if appCtx != nil {
				panelJustShown.Store(true)
				app.OnPanelShown() // resume probes + immediate refresh
				wailsRuntime.WindowShow(appCtx)
				if screens, err := wailsRuntime.ScreenGetAll(appCtx); err == nil {
					for _, s := range screens {
						if s.IsPrimary {
							x := s.Size.Width - 280 - 16    // 16px right margin
							y := s.Size.Height - 380 - 56   // 56px for taskbar
							wailsRuntime.WindowSetPosition(appCtx, x, y)
							break
						}
					}
				}
			}
		}
	}()

	// Graceful shutdown watcher
	go func() {
		<-shutdownCh
		log.Println("[DEBUG] Shutdown signal received from systray")
		if appCtx != nil {
			wailsRuntime.Quit(appCtx)
		}
	}()

	log.Println("[DEBUG] Starting Wails...")
	err := wails.Run(&options.App{
		Title:       "SDWAN Panel",
		Width:       280,
		Height:      380,
		Frameless:   true,
		StartHidden: true,

		AssetServer: &assetserver.Options{
			Assets: assets,
		},

		OnStartup: func(ctx context.Context) {
			log.Println("[DEBUG] OnStartup called")
			appCtx = ctx
			app.startup(ctx)
		},
		OnDomReady: func(ctx context.Context) {
			log.Println("[DEBUG] OnDomReady called — frontend loaded")
		},
		OnShutdown: func(ctx context.Context) {
			log.Println("[DEBUG] OnShutdown called")
			app.Shutdown()
		},

		Bind: []interface{}{app},
	})

	if err != nil {
		log.Fatalf("Wails error: %v", err)
	}
	log.Println("[DEBUG] Wails.Run returned normally")
}

// safeSystray wraps startSysTray with panic recovery.
func safeSystray() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("FATAL SYSTRAY PANIC: %v\n%s", r, debug.Stack())
		}
	}()
	startSysTray()
}
