package core

import (
	"errors"
	"testing"

	protocol "sdwan-go/pkg/protocol"
)

type fakeTunDevice struct {
	name       string
	closeCount int
}

func (t *fakeTunDevice) Read([]byte) (int, error)    { return 0, nil }
func (t *fakeTunDevice) Write(p []byte) (int, error) { return len(p), nil }
func (t *fakeTunDevice) Name() string                { return t.name }
func (t *fakeTunDevice) Close() error                { t.closeCount++; return nil }

func TestSetupTUNFailedAddDoesNotDeleteRoute(t *testing.T) {
	tun := &fakeTunDevice{name: "utun42"}
	var deletes, adds int
	client := NewClient(&Config{})
	priorConflicts := []RouteConflict{{RouteNet: "192.0.2.0/24"}}
	client.SetRouteConflicts(priorConflicts)
	_, cleanup, err := setupTUNWithOps(&Config{TUNName: "ignored", MTU: 1400, RouteNet: "10.0.0.0/8"}, &protocol.OPENACKResult{LocalIP: "10.100.100.7", GatewayIP: "10.100.100.1"}, client, tunSetupOps{
		create:    func(string, int, string) (TunDevice, error) { return tun, nil },
		setIP:     func(string, string, string) error { return nil },
		addRoute:  func(string, string, string) error { adds++; return errors.New("File exists") },
		delRoute:  func(string, string, string) { deletes++ },
		close:     func(d TunDevice, _ string) { _ = d.Close() },
		conflicts: func(string, string) []RouteConflict { return []RouteConflict{{RouteNet: "10.0.0.0/8"}} },
	})
	if err == nil || cleanup != nil || adds != 1 || deletes != 0 || tun.closeCount != 1 || client.TUN != nil || len(client.routeConflicts) != len(priorConflicts) {
		t.Fatalf("err=%v cleanup=%v adds=%d deletes=%d closes=%d tun=%v conflicts=%v", err, cleanup != nil, adds, deletes, tun.closeCount, client.TUN, client.routeConflicts)
	}
}

func TestSetupTUNIPFailureDoesNotPublishTUN(t *testing.T) {
	tun := &fakeTunDevice{name: "utun42"}
	client := NewClient(&Config{})
	_, cleanup, err := setupTUNWithOps(&Config{}, &protocol.OPENACKResult{LocalIP: "10.100.100.7", GatewayIP: "10.100.100.1"}, client, tunSetupOps{
		create: func(string, int, string) (TunDevice, error) { return tun, nil },
		setIP:  func(string, string, string) error { return errors.New("ifconfig failed") },
		close:  func(d TunDevice, _ string) { _ = d.Close() },
	})
	if err == nil || cleanup != nil || client.TUN != nil || tun.closeCount != 1 {
		t.Fatalf("err=%v cleanup=%v tun=%v closes=%d", err, cleanup != nil, client.TUN, tun.closeCount)
	}
}

func TestSetupTUNRejectsInvalidAddressesBeforeCreate(t *testing.T) {
	created := false
	_, cleanup, err := setupTUNWithOps(&Config{}, &protocol.OPENACKResult{LocalIP: "0.0.0.0", GatewayIP: "10.100.100.1"}, NewClient(&Config{}), tunSetupOps{
		create: func(string, int, string) (TunDevice, error) { created = true; return nil, nil },
	})
	if err == nil || cleanup != nil || created {
		t.Fatalf("err=%v cleanup=%v created=%t; want validation failure before create", err, cleanup != nil, created)
	}
}

func TestSetupTUNCleanupOwnsRouteAndTUNOnce(t *testing.T) {
	tun := &fakeTunDevice{name: "utun42"}
	var deletes int
	client := NewClient(&Config{})
	name, cleanup, err := setupTUNWithOps(&Config{MTU: 1400, RouteNet: "10.0.0.0/8"}, &protocol.OPENACKResult{LocalIP: "10.100.100.7", GatewayIP: "10.100.100.1"}, client, tunSetupOps{
		create: func(string, int, string) (TunDevice, error) { return tun, nil }, setIP: func(string, string, string) error { return nil },
		addRoute: func(string, string, string) error { return nil }, delRoute: func(string, string, string) { deletes++ },
		close: func(d TunDevice, name string) {
			if name != "utun42" {
				t.Errorf("close name = %q", name)
			}
			_ = d.Close()
		},
	})
	if err != nil || name != "utun42" || client.TUN != tun {
		t.Fatalf("setup = %q, %v", name, err)
	}
	cleanup()
	cleanup()
	if deletes != 1 || tun.closeCount != 1 {
		t.Fatalf("deletes=%d closes=%d, want one each", deletes, tun.closeCount)
	}
}
