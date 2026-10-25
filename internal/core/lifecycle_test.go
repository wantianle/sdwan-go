package core

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	protocol "sdwan-go/pkg/protocol"
)

func TestStoppingRejectsLifecyclePublicationAndControl(t *testing.T) {
	c := NewClient(&Config{Server: "original", MTU: 1400})
	c.Close()

	if c.SetPaused(true) || c.Paused() {
		t.Fatal("pause was accepted after stopping")
	}
	if c.SetTunnelConfig(&protocol.OPENACKResult{LocalIP: "10.100.100.2"}) {
		t.Fatal("tunnel config was published after stopping")
	}
	if c.publishTUN(&fakeTunDevice{name: "late"}) {
		t.Fatal("TUN was published after stopping")
	}
	if _, err := c.SwitchServer(&Config{}); err == nil {
		t.Fatal("switch was admitted after stopping")
	}
}

func TestStartupFinalPublicationRejectsStopRace(t *testing.T) {
	c := NewClient(&Config{})
	c.beforeFinalReady = c.Close
	if c.publishStartupReady() {
		t.Fatal("startup ready was published after stop")
	}
	if c.tunnelReady.Load() {
		t.Fatal("ready remained published after stop")
	}
}

func TestStartRejectsShutdownBeforeInitialLoopLaunch(t *testing.T) {
	c := NewClient(&Config{})
	c.startDelay = 0
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9})
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{conn: conn, done: make(chan struct{})}
	c.setSession(s)
	loopsStarted := make(chan struct{}, 1)
	c.onSessionLoopsStarted = func() { loopsStarted <- struct{}{} }
	entered := make(chan struct{})
	release := make(chan struct{})
	c.beforeInitialLaunch = func() {
		close(entered)
		<-release
	}
	if !c.beginLifecycle() {
		t.Fatal("failed to admit initial startup")
	}
	startResult := make(chan error, 1)
	go func() { defer c.endLifecycle(); startResult <- c.Start() }()
	<-entered
	closed := make(chan struct{})
	go func() { c.Close(); close(closed) }()
	deadline := time.Now().Add(time.Second)
	for !c.isStopped() {
		if time.Now().After(deadline) {
			t.Fatal("Close did not begin stopping before initial launch was released")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	if !errors.Is(<-startResult, errDaemonStopping) {
		t.Fatal("Start did not report stopping after the initial launch boundary")
	}
	<-closed
	if c.reconnectStarted.Load() {
		t.Fatal("Start launched reconnect worker after shutdown")
	}
	if c.packetPumpStarted.Load() {
		t.Fatal("Start launched packet pump after shutdown")
	}
	select {
	case <-loopsStarted:
		t.Fatal("Start launched session loops after shutdown")
	default:
	}
	select {
	case <-s.Done():
	case <-time.After(time.Second):
		t.Fatal("shutdown did not close the initial session")
	}
}

func TestStartRejectsShutdownDuringDelayBeforePumpAndReconnect(t *testing.T) {
	c := NewClient(&Config{})
	c.startDelay = time.Hour
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9})
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{conn: conn, done: make(chan struct{})}
	c.setSession(s)
	loopsStarted := make(chan struct{}, 1)
	c.onSessionLoopsStarted = func() { loopsStarted <- struct{}{} }
	if !c.beginLifecycle() {
		t.Fatal("failed to admit initial startup")
	}
	startResult := make(chan error, 1)
	go func() { defer c.endLifecycle(); startResult <- c.Start() }()
	select {
	case <-loopsStarted:
	case <-time.After(time.Second):
		t.Fatal("Start did not launch initial session loops")
	}
	closed := make(chan struct{})
	go func() { c.Close(); close(closed) }()
	if !errors.Is(<-startResult, errDaemonStopping) {
		t.Fatal("Start did not report stopping during protocol delay")
	}
	<-closed
	if c.packetPumpStarted.Load() || c.reconnectStarted.Load() {
		t.Fatal("Start launched packet pump or reconnect worker after shutdown during delay")
	}
}

func TestSwitchFinalPublicationRejectsStopRace(t *testing.T) {
	c := NewClient(&Config{})
	s := &Session{done: make(chan struct{})}
	c.setSession(s) // models the completed guarded session/config swap.
	c.beforeFinalReady = c.Close
	if c.publishSwitchedSession(s) {
		t.Fatal("switch loops/ready were published after stop")
	}
	select {
	case <-s.Done():
	case <-time.After(time.Second):
		t.Fatal("switched session was not closed after rejected publication")
	}
}

func TestCloseClearsReady(t *testing.T) {
	c := NewClient(&Config{})
	if !c.publishReady() {
		t.Fatal("initial ready publication failed")
	}
	c.Close()
	if c.tunnelReady.Load() {
		t.Fatal("Close did not clear ready")
	}
}

func TestControlRejectsNewOperationsWhileStopping(t *testing.T) {
	c := NewClient(&Config{Server: "original"})
	c.Close()
	called := false
	mux := newControlMux(c, func(*Config) (*protocol.OPENACKResult, error) {
		called = true
		return nil, nil
	}, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/switch", strings.NewReader(`{"server":"next"}`))
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusServiceUnavailable || called {
		t.Fatalf("status=%d switchCalled=%t; want 503 and false", res.Code, called)
	}
}

func TestControlMapsSwitchStoppingErrorToServiceUnavailable(t *testing.T) {
	c := NewClient(&Config{Server: "original"})
	mux := newControlMux(c, func(*Config) (*protocol.OPENACKResult, error) {
		return nil, fmt.Errorf("switch rejected: %w", errDaemonStopping)
	}, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/switch", strings.NewReader(`{"server":"next"}`))
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", res.Code)
	}
}

func TestControlMapsInterruptedSwitchToServiceUnavailable(t *testing.T) {
	server, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	addr := server.LocalAddr().(*net.UDPAddr)
	c := NewClient(&Config{Server: addr.IP.String(), Port: addr.Port, Username: "u", Password: "p", MTU: 1400})
	c.SetTunnelConfig(&protocol.OPENACKResult{LocalIP: "10.100.100.2", GatewayIP: "10.100.100.1"})
	mux := newControlMux(c, c.SwitchServer, nil)
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/switch", strings.NewReader(`{"server":"127.0.0.1"}`))
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, req)
		response <- res
	}()
	buf := make([]byte, 2048)
	if _, _, err := server.ReadFromUDP(buf); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { c.Close(); close(done) }()
	select {
	case res := <-response:
		if res.Code != http.StatusServiceUnavailable {
			t.Fatalf("status=%d, want 503: %s", res.Code, res.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("interrupted switch control request did not return")
	}
	<-done
}

func TestControlPauseRejectsStopRace(t *testing.T) {
	c := NewClient(&Config{})
	c.beforePausePublish = c.Close
	mux := newControlMux(c, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/pause", strings.NewReader(`{"pause":true}`))
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", res.Code)
	}
}

func TestConfigSnapshotsAreImmutable(t *testing.T) {
	cfg := &Config{Server: "original", MTU: 1400}
	c := NewClient(cfg)
	cfg.Server = "caller-mutated"

	first := c.currentConfig()
	if first.Server != "original" {
		t.Fatalf("client config = %q, want original", first.Server)
	}
	first.Server = "snapshot-mutated"
	if got := c.currentConfig().Server; got != "original" {
		t.Fatalf("client config changed through snapshot: %q", got)
	}
}

func TestTUNPublicationAndClearUseSnapshot(t *testing.T) {
	c := NewClient(&Config{})
	tun := &fakeTunDevice{name: "snapshot-tun"}
	if !c.publishTUN(tun) {
		t.Fatal("initial TUN publication failed")
	}
	if got := c.currentTUN(); got != tun {
		t.Fatalf("TUN snapshot = %v, want %v", got, tun)
	}
	c.clearTUN(tun)
	if got := c.currentTUN(); got != nil {
		t.Fatalf("TUN was not cleared: %v", got)
	}
}

func TestLateStartupTUNCannotPublishAfterStop(t *testing.T) {
	c := NewClient(&Config{})
	c.Close()
	tun := &fakeTunDevice{name: "late-tun"}
	var deleted, closed bool
	_, cleanup, err := setupTUNWithOps(
		&Config{TUNName: tun.name, MTU: 1400, RouteNet: "10.0.0.0/8"},
		&protocol.OPENACKResult{LocalIP: "10.100.100.2", GatewayIP: "10.100.100.1"}, c,
		tunSetupOps{
			create:   func(string, int, string) (TunDevice, error) { return tun, nil },
			setIP:    func(string, string, string) error { return nil },
			addRoute: func(string, string, string) error { return nil },
			delRoute: func(string, string, string) { deleted = true },
			close:    func(TunDevice, string) { closed = true },
		},
	)
	if !errors.Is(err, errDaemonStopping) {
		t.Fatalf("setup error = %v, want stopping error", err)
	}
	if cleanup != nil || c.currentTUN() != nil || !deleted || !closed {
		t.Fatalf("cleanup=%v tun=%v deleted=%t closed=%t", cleanup != nil, c.currentTUN(), deleted, closed)
	}
}

func TestCloseWaitsForAdmittedLifecycleWork(t *testing.T) {
	c := NewClient(&Config{})
	if !c.beginLifecycle() {
		t.Fatal("initial lifecycle admission failed")
	}
	done := make(chan struct{})
	go func() { c.Close(); close(done) }()

	select {
	case <-done:
		t.Fatal("Close completed before lifecycle work ended")
	case <-time.After(20 * time.Millisecond):
	}
	c.endLifecycle()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not complete after lifecycle work ended")
	}
}
