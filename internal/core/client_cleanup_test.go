package core

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestClientTUNCleanupRunsOnceAndOutsideRegistrationLock(t *testing.T) {
	c := NewClient(&Config{})
	var calls int
	c.SetTUNCleanup(func() { calls++; c.SetTUNCleanup(nil) })
	c.Close()
	c.Close()
	if calls != 1 {
		t.Fatalf("cleanup calls = %d, want 1", calls)
	}
}

func TestClientTUNCleanupConcurrentSetAndCloseRunsOnce(t *testing.T) {
	c := NewClient(&Config{})
	start := make(chan struct{})
	var wg sync.WaitGroup
	var calls atomic.Int32
	cleanup := func() {
		calls.Add(1)
		// Re-entering registration proves this callback holds neither cleanup lock.
		c.SetTUNCleanup(nil)
	}
	wg.Add(2)
	go func() { defer wg.Done(); <-start; c.SetTUNCleanup(cleanup) }()
	go func() { defer wg.Done(); <-start; c.Close() }()
	close(start)
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("cleanup calls = %d, want 1", got)
	}
}

func TestClientTUNCleanupRegisteredAfterCloseRunsImmediately(t *testing.T) {
	c := NewClient(&Config{})
	c.Close()
	called := false
	c.SetTUNCleanup(func() { called = true; c.SetTUNCleanup(nil) })
	if !called {
		t.Fatal("cleanup registered after Close was not invoked")
	}
}
