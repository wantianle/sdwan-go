package core

import (
	"testing"
)

func TestCloneConfig(t *testing.T) {
	orig := &Config{
		Server:   "test.example.com",
		Port:     10010,
		Username: "user",
		Password: "pass",
		MTU:      1436,
		Encrypt:  1,
	}

	cpy := cloneConfig(orig)
	if cpy == orig {
		t.Fatal("cloneConfig returned same pointer, want a copy")
	}
	if cpy.Server != orig.Server {
		t.Errorf("Server: got %q, want %q", cpy.Server, orig.Server)
	}
	if cpy.Port != orig.Port {
		t.Errorf("Port: got %d, want %d", cpy.Port, orig.Port)
	}

	// Mutate copy — original must be unaffected.
	cpy.Server = "modified"
	if orig.Server == cpy.Server {
		t.Error("mutation of copy affected original")
	}
}

func TestCloneConfigNil(t *testing.T) {
	if cloneConfig(nil) != nil {
		t.Error("cloneConfig(nil) should return nil")
	}
}

func TestCheckTunnelCompatibleOk(t *testing.T) {
	cfg := &Config{MTU: 1436, Encrypt: 0}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.SetTunnelConfig(&OPENACKResult{
		LocalIP:   "10.0.0.2",
		GatewayIP: "10.0.0.1",
		MTU:       1436,
	})

	// Same IPs, same MTU → OK
	err = c.checkTunnelCompatible(&OPENACKResult{
		LocalIP:   "10.0.0.2",
		GatewayIP: "10.0.0.1",
		MTU:       1436,
	})
	if err != nil {
		t.Errorf("expected compatible, got: %v", err)
	}
}

func TestCheckTunnelCompatibleDifferentLocalIP(t *testing.T) {
	cfg := &Config{MTU: 1436, Encrypt: 0}
	c, _ := NewClient(cfg)
	c.SetTunnelConfig(&OPENACKResult{
		LocalIP:   "10.0.0.2",
		GatewayIP: "10.0.0.1",
	})

	err := c.checkTunnelCompatible(&OPENACKResult{
		LocalIP:   "10.0.0.99",
		GatewayIP: "10.0.0.1",
	})
	if err == nil {
		t.Fatal("expected incompatible, got nil")
	}
}

func TestCheckTunnelCompatibleDifferentGateway(t *testing.T) {
	cfg := &Config{MTU: 1436, Encrypt: 0}
	c, _ := NewClient(cfg)
	c.SetTunnelConfig(&OPENACKResult{
		LocalIP:   "10.0.0.2",
		GatewayIP: "10.0.0.1",
	})

	err := c.checkTunnelCompatible(&OPENACKResult{
		LocalIP:   "10.0.0.2",
		GatewayIP: "10.0.0.254",
	})
	if err == nil {
		t.Fatal("expected incompatible, got nil")
	}
}

func TestCheckTunnelCompatibleMTUMismatch(t *testing.T) {
	cfg := &Config{MTU: 1436, Encrypt: 0}
	c, _ := NewClient(cfg)
	c.SetTunnelConfig(&OPENACKResult{
		LocalIP:   "10.0.0.2",
		GatewayIP: "10.0.0.1",
	})

	err := c.checkTunnelCompatible(&OPENACKResult{
		LocalIP:   "10.0.0.2",
		GatewayIP: "10.0.0.1",
		MTU:       1200, // different from config's 1436
	})
	if err == nil {
		t.Fatal("expected MTU mismatch error, got nil")
	}
}

func TestCheckTunnelCompatibleNoBaseline(t *testing.T) {
	cfg := &Config{MTU: 1436, Encrypt: 0}
	c, _ := NewClient(cfg)
	// No SetTunnelConfig call — baseline is nil.

	err := c.checkTunnelCompatible(&OPENACKResult{
		LocalIP:   "10.0.0.2",
		GatewayIP: "10.0.0.1",
	})
	if err == nil {
		t.Fatal("expected error when no baseline set, got nil")
	}
}

func TestSessionCloseIdempotent(t *testing.T) {
	// Create a session manually with an in-memory pipe so Close can be
	// called without a real network connection.
	s := &Session{
		done: make(chan struct{}),
	}
	s.Close()
	// Second Close must not panic.
	s.Close()
	// After close, done should be closed.
	select {
	case <-s.done:
	default:
		t.Error("done channel not closed after Close")
	}
}

func TestSwitchServerNilConfig(t *testing.T) {
	c, err := NewClient(&Config{Server: "old", Username: "u", Password: "p", Port: 10010, MTU: 1436})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SwitchServer(nil); err == nil {
		t.Fatal("expected error for nil config, got nil")
	}
}

func TestIsCurrentSession(t *testing.T) {
	cfg := &Config{Server: "test", Username: "u", Password: "p", Port: 10010, MTU: 1436}
	c, _ := NewClient(cfg)

	// No session set yet — nothing is current.
	if c.isCurrentSession(&Session{done: make(chan struct{})}) {
		t.Error("no session set, isCurrentSession should be false")
	}

	// Create a session and set it.
	s := &Session{done: make(chan struct{})}
	c.setSession(s)
	if !c.isCurrentSession(s) {
		t.Error("session should be current after setSession")
	}
	if c.isCurrentSession(&Session{done: make(chan struct{})}) {
		t.Error("different pointer should not match as current")
	}

	// Swap to nil.
	c.setSession(nil)
	if c.isCurrentSession(s) {
		t.Error("after setting nil, old session should not be current")
	}
}

func TestStartWithoutSession(t *testing.T) {
	cfg := &Config{Server: "test", Username: "u", Password: "p", Port: 10010, MTU: 1436}
	c, _ := NewClient(cfg)
	err := c.Start()
	if err == nil {
		t.Fatal("expected error when calling Start without a session")
	}
}
