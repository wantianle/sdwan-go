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

const (
	panelWidth  = 280
	panelHeight = 380
	panelMargin = 16
)

func clamp(value, minValue, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

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

	go safeSystray()

	// Show-panel signal watcher — positions panel at bottom-right
	go func() {
		for range trayShowCh {
			log.Println("[DEBUG] Show panel signal received")
			if appCtx != nil {
				panelJustShown.Store(true)
				app.OnPanelShown() // resume probes + immediate refresh
				wailsRuntime.WindowShow(appCtx)
				windowWidth, windowHeight := wailsRuntime.WindowGetSize(appCtx)
				if windowWidth <= 0 {
					windowWidth = panelWidth
				}
				if windowHeight <= 0 {
					windowHeight = panelHeight
				}

				if screens, err := wailsRuntime.ScreenGetAll(appCtx); err == nil {
					for _, s := range screens {
						if s.IsPrimary {
							logicalLeft := 0
							logicalTop := 0
							logicalRight := s.Size.Width
							logicalBottom := s.Size.Height

							if workArea, ok := primaryWorkArea(); ok && workArea.MonitorWidth > 0 && workArea.MonitorHeight > 0 {
								scaleX := float64(s.Size.Width) / float64(workArea.MonitorWidth)
								scaleY := float64(s.Size.Height) / float64(workArea.MonitorHeight)
								logicalLeft = int(float64(workArea.WorkLeft-workArea.MonitorLeft) * scaleX)
								logicalTop = int(float64(workArea.WorkTop-workArea.MonitorTop) * scaleY)
								logicalRight = int(float64(workArea.WorkRight-workArea.MonitorLeft) * scaleX)
								logicalBottom = int(float64(workArea.WorkBottom-workArea.MonitorTop) * scaleY)
							}

							x := logicalRight - windowWidth - panelMargin
							y := logicalBottom - windowHeight - panelMargin
							x = clamp(x, logicalLeft+panelMargin, logicalRight-windowWidth)
							y = clamp(y, logicalTop+panelMargin, logicalBottom-windowHeight)
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
		Width:       panelWidth,
		Height:      panelHeight,
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
