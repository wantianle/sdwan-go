package main

import (
	"context"
	"log"
	"sync/atomic"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"sdwan-panel/core"
)

// panelJustShown is set to true right before ShowPanel so the first blur
// event (which fires immediately because the tray menu still has focus)
// does not hide the panel.
var panelJustShown atomic.Bool

// App serves as the bridge between the Wails frontend and the
// SD-WAN core manager.
type App struct {
	ctx     context.Context
	manager *core.SdwanManager
}

func NewApp() *App {
	return &App{
		manager: core.GetManager(),
	}
}

// startup is called by Wails when the application starts.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	go a.manager.WatchIwanConf(func() {
		log.Println("[APP] iwan.conf changed, restarting...")
		a.manager.Reload()
	})
}

// --- Exported methods (called from frontend JS) ----------------------------

func (a *App) GetStatus() map[string]interface{} {
	return a.manager.GetStatus()
}

func (a *App) GetServers() []map[string]string {
	return a.manager.GetServers()
}

func (a *App) ToggleConnection() bool {
	return a.manager.ToggleConnection()
}

func (a *App) SelectServer(id string) bool {
	return a.manager.SelectServer(id)
}

func (a *App) EditConfig() {
	_ = a.manager.EditConfig()
}

func (a *App) Reload() bool {
	return a.manager.Reload()
}

func (a *App) HidePanel() {
	// Suspend probes when panel is hidden — no point wasting CPU
	a.manager.SuspendProbes()

	if panelJustShown.Swap(false) {
		return
	}
	if a.ctx != nil {
		runtime.WindowHide(a.ctx)
	}
}

// OnPanelShown is called when the panel is shown (from tray double-click).
// It resumes latency probes and triggers an immediate refresh.
func (a *App) OnPanelShown() {
	a.manager.ResumeProbes()
}

func (a *App) Shutdown() {
	a.manager.Shutdown()
}
