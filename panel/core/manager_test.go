//go:build windows

package core

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	controlapi "sdwan-go/pkg/controlapi"
)

func testManager() *SdwanManager {
	m := &SdwanManager{
		config:         defaultConfig(),
		serverLatency:  make(map[string]int64),
		stopCh:         make(chan struct{}),
		daemonPollStop: make(chan struct{}, 1),
		lifecycleSlot:  make(chan struct{}, 1),
	}
	m.lifecycleSlot <- struct{}{}
	return m
}

func TestLifecycleAdmissionSerializesWork(t *testing.T) {
	m := testManager()
	first, ok := m.beginLifecycle()
	if !ok {
		t.Fatal("first lifecycle operation was rejected")
	}
	entered := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done, ok := m.beginLifecycle()
		if !ok {
			return
		}
		close(entered)
		done()
	}()
	select {
	case <-entered:
		t.Fatal("second lifecycle operation overlapped the first")
	case <-time.After(30 * time.Millisecond):
	}
	first()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("second lifecycle operation was not admitted after first completed")
	}
	<-finished
}

func TestStoppingBlocksLateLifecycleAndDaemonStart(t *testing.T) {
	m := testManager()
	m.mu.Lock()
	m.stopping = true
	m.mu.Unlock()
	if _, ok := m.beginLifecycle(); ok {
		t.Fatal("stopping manager admitted lifecycle work")
	}
	if m.ensureDaemonRunning() {
		t.Fatal("stopping manager attempted to ensure daemon")
	}
}

func TestCallbackSnapshotRunsOutsideManagerLock(t *testing.T) {
	m := testManager()
	var called bool
	m.SetStateChangeCallback(func() {
		m.SetStateChangeCallback(nil) // re-entrant lock acquisition must not deadlock
		called = true
	})
	m.notifyStateChange()
	if !called {
		t.Fatal("state callback was not invoked")
	}
}

func TestLifecycleCallbackCanReenterSerializer(t *testing.T) {
	m := testManager()
	done, ok := m.beginLifecycle()
	if !ok {
		t.Fatal("lifecycle operation was rejected")
	}
	reentered := make(chan struct{})
	m.SetStateChangeCallback(func() {
		m.SetStateChangeCallback(nil)
		inner, ok := m.beginLifecycle()
		if !ok {
			t.Error("callback lifecycle re-entry was rejected")
			return
		}
		inner()
		close(reentered)
	})
	finished := make(chan struct{})
	go func() { done(); close(finished) }()
	select {
	case <-reentered:
	case <-time.After(time.Second):
		t.Fatal("callback deadlocked while re-entering lifecycle serializer")
	}
	<-finished
}

func TestQueuedLifecycleSkipsAfterShutdown(t *testing.T) {
	m := testManager()
	first, ok := m.beginLifecycle()
	if !ok {
		t.Fatal("first lifecycle operation was rejected")
	}
	ran := make(chan struct{}, 1)
	if !m.startLifecycle(func() { ran <- struct{}{} }) {
		t.Fatal("queued work was not admitted")
	}
	shutdownDone := make(chan struct{})
	go func() { m.Shutdown(); close(shutdownDone) }()
	time.Sleep(30 * time.Millisecond)
	first()
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish")
	}
	select {
	case <-ran:
		t.Fatal("queued lifecycle work ran after shutdown admission closed")
	default:
	}
}

func TestDaemonStartBoundaryRejectsShutdown(t *testing.T) {
	m := testManager()
	dir := t.TempDir()
	m.exeDir, m.iwanPath = dir, dir+"\\iwan.conf"
	if err := os.WriteFile(filepath.Join(dir, "sdwan-windows-amd64.exe"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.iwanPath, []byte("server=minieye.9966.org\n"), 0600); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	oldBefore, oldStart := beforeDaemonStart, startCommand
	defer func() { beforeDaemonStart, startCommand = oldBefore, oldStart }()
	beforeDaemonStart = func() { close(entered); <-release }
	started := 0
	startCommand = func(*exec.Cmd) error { started++; return errors.New("test start") }
	go m.startDaemon()
	<-entered
	m.mu.Lock()
	m.stopping = true
	m.mu.Unlock()
	close(release)
	time.Sleep(30 * time.Millisecond)
	if started != 0 {
		t.Fatal("daemon process start crossed shutdown boundary")
	}
}

func TestDaemonStartDoesNotHoldManagerLock(t *testing.T) {
	m := testManager()
	dir := t.TempDir()
	m.exeDir, m.iwanPath = dir, filepath.Join(dir, "iwan.conf")
	if err := os.WriteFile(filepath.Join(dir, "sdwan-windows-amd64.exe"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.iwanPath, []byte("server=minieye.9966.org\n"), 0600); err != nil {
		t.Fatal(err)
	}
	oldStart := startCommand
	defer func() { startCommand = oldStart }()
	entered := make(chan struct{})
	startCommand = func(*exec.Cmd) error {
		m.mu.Lock()
		m.mu.Unlock()
		close(entered)
		return errors.New("test start")
	}
	m.startDaemon()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("startCommand could not acquire manager lock")
	}
}

func TestAutoConnectProbeCallbackReentersLifecycle(t *testing.T) {
	m := testManager()
	oldProbe, oldEnsure := autoConnectProbeOnce, autoConnectEnsureDaemon
	defer func() { autoConnectProbeOnce, autoConnectEnsureDaemon = oldProbe, oldEnsure }()
	reentered := make(chan struct{})
	m.SetStateChangeCallback(func() {
		m.SetStateChangeCallback(nil)
		done, ok := m.beginLifecycle()
		if !ok {
			t.Error("probe callback lifecycle re-entry rejected")
			return
		}
		done()
		close(reentered)
	})
	autoConnectProbeOnce = func(*SdwanManager) {}
	autoConnectEnsureDaemon = func(*SdwanManager) bool { return false }
	m.AutoConnect()
	select {
	case <-reentered:
	case <-time.After(time.Second):
		t.Fatal("probe callback deadlocked in lifecycle slot")
	}
}

func TestStoppingAutoConnectSkipsProbeCallbackAndDaemonEnsure(t *testing.T) {
	m := testManager()
	oldProbe, oldEnsure := autoConnectProbeOnce, autoConnectEnsureDaemon
	defer func() { autoConnectProbeOnce, autoConnectEnsureDaemon = oldProbe, oldEnsure }()
	probeCalled, ensureCalled, callbackCalled := false, false, false
	autoConnectProbeOnce = func(*SdwanManager) { probeCalled = true }
	autoConnectEnsureDaemon = func(*SdwanManager) bool { ensureCalled = true; return false }
	m.SetStateChangeCallback(func() { callbackCalled = true })
	m.mu.Lock()
	m.stopping = true
	m.mu.Unlock()
	m.AutoConnect()
	time.Sleep(30 * time.Millisecond)
	if probeCalled || ensureCalled || callbackCalled {
		t.Fatalf("stopping AutoConnect ran probe=%v ensure=%v callback=%v", probeCalled, ensureCalled, callbackCalled)
	}
}

func TestPollerLaunchRejectedAfterStopping(t *testing.T) {
	m := testManager()
	m.mu.Lock()
	m.stopping = true
	m.mu.Unlock()
	m.startDaemonPoller()
	if m.daemonPollerOn.Load() {
		t.Fatal("poller launched after stopping")
	}
}

func TestPollerLaunchDoesNotPublishWhenShutdownWinsPreLaunch(t *testing.T) {
	m := testManager()
	entered, release := make(chan struct{}), make(chan struct{})
	workerStarted := make(chan struct{}, 1)
	oldBefore, oldWorker := beforeDaemonPollerLaunch, daemonPollerWorkerStarted
	defer func() { beforeDaemonPollerLaunch, daemonPollerWorkerStarted = oldBefore, oldWorker }()
	beforeDaemonPollerLaunch = func() { close(entered); <-release }
	daemonPollerWorkerStarted = func() { workerStarted <- struct{}{} }
	launchDone := make(chan struct{})
	go func() { m.startDaemonPoller(); close(launchDone) }()
	<-entered
	m.mu.Lock()
	m.stopping = true
	m.mu.Unlock()
	close(release)
	<-launchDone
	if m.daemonPollerOn.Load() {
		t.Fatal("poller publication survived shutdown")
	}
	select {
	case <-workerStarted:
		t.Fatal("poller worker was published after shutdown")
	default:
	}
}

func TestInFlightPollDoesNotPublishAfterStopping(t *testing.T) {
	m := testManager()
	m.token, m.controlAddr = "token", "ignored"
	oldStatus := controlStatusWithTimeout
	defer func() { controlStatusWithTimeout = oldStatus }()
	entered, release := make(chan struct{}), make(chan struct{})
	controlStatusWithTimeout = func(string, string, time.Duration) (*controlapi.StatusResult, error) {
		close(entered)
		<-release
		return &controlapi.StatusResult{State: "running"}, nil
	}
	done := make(chan struct{})
	go func() { m.pollDaemonStatusOnce(); close(done) }()
	<-entered
	m.mu.Lock()
	m.stopping = true
	m.mu.Unlock()
	close(release)
	<-done
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.connected || m.state == "running" {
		t.Fatal("in-flight poll published after stopping")
	}
}

func TestPollReadyDoesNotPublishAfterStopping(t *testing.T) {
	m := testManager()
	oldStatus, oldInterval := controlStatusWithTimeout, pollReadyInterval
	defer func() { controlStatusWithTimeout, pollReadyInterval = oldStatus, oldInterval }()
	pollReadyInterval = 0
	controlStatusWithTimeout = func(string, string, time.Duration) (*controlapi.StatusResult, error) {
		m.mu.Lock()
		m.stopping = true
		m.mu.Unlock()
		return &controlapi.StatusResult{State: "running"}, nil
	}
	if m.pollDaemonReady("token", "ignored") {
		t.Fatal("poll published ready after stopping")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.connected || m.state == "running" {
		t.Fatal("late poll changed cached connection state")
	}
}

func TestSwitch503PreservesCachedState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/status" {
			_, _ = w.Write([]byte(`{"state":"running"}`))
			return
		}
		http.Error(w, "daemon stopping", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	m := testManager()
	m.controlAddr = strings.TrimPrefix(server.URL, "http://")
	m.token = "test-token"
	m.state = "running"
	m.connected = true
	if m.SelectServer("2") {
		t.Fatal("switch unexpectedly succeeded")
	}
	m.mu.Lock()
	state, connected, selected := m.state, m.connected, m.config.CurrentServer
	m.mu.Unlock()
	if state != "running" || !connected || selected != "1" {
		t.Fatalf("503 changed state to %q, connected=%v, selected=%q", state, connected, selected)
	}
}

func TestShutdownWaitsForAdmittedLifecycleWork(t *testing.T) {
	m := testManager()
	done, ok := m.beginLifecycle()
	if !ok {
		t.Fatal("lifecycle operation was rejected")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	shutdownDone := make(chan struct{})
	go func() {
		defer wg.Done()
		m.Shutdown()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned before admitted lifecycle work completed")
	case <-time.After(30 * time.Millisecond):
	}
	done()
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not return after lifecycle work completed")
	}
	wg.Wait()
}
