package main

import (
	"context"
	"embed"
	"log"
	"os"
	"runtime/debug"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed all:frontend
var assets embed.FS

// appCtx holds the Wails runtime context for window operations.
var appCtx context.Context

const (
	panelWidth  = 280
	panelHeight = 380
)

func main() {
	// Crash recovery — log panics instead of silently exiting
	defer func() {
		if r := recover(); r != nil {
			log.Printf("FATAL PANIC: %v\n%s", r, debug.Stack())
			os.Exit(1)
		}
	}()

	app := NewApp()

	if f, err := os.Create("panel.log"); err == nil {
		log.SetOutput(f)
		defer f.Close()
	}
	log.Println("SDWAN Panel starting...")
	log.Println("[DEBUG] Panel version 1.0.0")

	// Show-panel signal watcher — positions panel at screen center
	go func() {
		for range trayShowCh {
			showPanel("tray signal", appCtx, app)
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
		Width:       panelWidth,
		Height:      panelHeight,
		Frameless:   true,
		StartHidden: true,
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: "sdwan-panel",
			OnSecondInstanceLaunch: func(data options.SecondInstanceData) {
				log.Println("[DEBUG] Second panel instance requested; showing existing window")
				go showPanel("second instance", appCtx, app)
			},
		},

		AssetServer: &assetserver.Options{
			Assets: assets,
		},

		OnStartup: func(ctx context.Context) {
			log.Println("[DEBUG] OnStartup called")
			appCtx = ctx
			app.startup(ctx)
			go safeSystray()
		},
		OnDomReady: func(ctx context.Context) {
			log.Println("[DEBUG] OnDomReady called — frontend loaded")
			go func() {
				time.Sleep(300 * time.Millisecond)
				showPanel("first DOM ready", ctx, app)
			}()
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

func showPanel(reason string, ctx context.Context, app *App) {
	log.Printf("[DEBUG] Show panel requested: %s", reason)
	if ctx == nil {
		log.Printf("[DEBUG] Show panel skipped: nil context (%s)", reason)
		return
	}
	wailsRuntime.WindowUnminimise(ctx)
	wailsRuntime.WindowShow(ctx)
	wailsRuntime.WindowCenter(ctx)
	wailsRuntime.WindowSetAlwaysOnTop(ctx, true)
	time.Sleep(150 * time.Millisecond)
	wailsRuntime.WindowSetAlwaysOnTop(ctx, false)
	app.OnPanelShown()
	log.Printf("[DEBUG] Show panel complete: %s", reason)
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
