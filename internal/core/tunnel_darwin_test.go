//go:build darwin

package core

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestDarwinTUNIPArgs(t *testing.T) {
	args, err := darwinTUNIPArgs("10.100.100.7", "10.100.100.1")
	if err != nil {
		t.Fatalf("darwinTUNIPArgs returned error: %v", err)
	}
	want := []string{"inet", "10.100.100.7", "10.100.100.1", "netmask", "255.255.255.0", "up"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("arguments = %q, want %q", args, want)
	}
}

func TestDarwinTUNIPArgsRejectsNonIPv4(t *testing.T) {
	for _, tc := range []struct{ name, local, peer string }{
		{"invalid local", "not-an-ip", "10.100.100.1"},
		{"IPv6 local", "2001:db8::1", "10.100.100.1"},
		{"unspecified local", "0.0.0.0", "10.100.100.1"},
		{"invalid peer", "10.100.100.7", "not-an-ip"},
		{"IPv6 peer", "10.100.100.7", "2001:db8::1"},
		{"unspecified peer", "10.100.100.7", "0.0.0.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := darwinTUNIPArgs(tc.local, tc.peer); err == nil {
				t.Fatal("darwinTUNIPArgs succeeded, want validation error")
			}
		})
	}
}

func TestSetDarwinMTUCommandAndOutput(t *testing.T) {
	err := setDarwinMTU("utun3", 1420, func(name string, args ...string) ([]byte, error) {
		if name != "ifconfig" || !reflect.DeepEqual(args, []string{"utun3", "mtu", "1420"}) {
			t.Errorf("command = %q %q", name, args)
		}
		return []byte("permission denied"), errors.New("exit status 1")
	})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error = %v, want command output", err)
	}
}

func TestSetDarwinTUNIPGatewayPeerCommandAndOutput(t *testing.T) {
	err := setDarwinTUNIP("utun2", "10.100.100.7/24", "10.100.100.1", func(name string, args ...string) ([]byte, error) {
		want := []string{"utun2", "inet", "10.100.100.7", "10.100.100.1", "netmask", "255.255.255.0", "up"}
		if name != "ifconfig" || !reflect.DeepEqual(args, want) {
			t.Errorf("command = %q %q, want %q %q", name, args, "ifconfig", want)
		}
		return []byte("ifconfig failed"), errors.New("exit status 1")
	})
	if err == nil || !strings.Contains(err.Error(), "ifconfig failed") {
		t.Fatalf("error = %v, want command output", err)
	}
}

func TestSetDarwinTUNIPRejectsInvalidCIDRWithoutCommand(t *testing.T) {
	called := false
	err := setDarwinTUNIP("utun2", "10.100.100.7/16", "10.100.100.1", func(string, ...string) ([]byte, error) {
		called = true
		return nil, nil
	})
	if err == nil || called {
		t.Fatalf("error = %v, command called = %t; want validation failure without command", err, called)
	}
}

func TestAddDarwinRouteCommandAndOutput(t *testing.T) {
	err := addDarwinRoute("10.0.0.0/8", "utun2", func(name string, args ...string) ([]byte, error) {
		want := []string{"-n", "add", "-net", "10.0.0.0/8", "-interface", "utun2"}
		if name != "route" || !reflect.DeepEqual(args, want) {
			t.Errorf("command = %q %q, want %q %q", name, args, "route", want)
		}
		return []byte("route exists"), errors.New("exit status 1")
	})
	if err == nil || !strings.Contains(err.Error(), "route exists") {
		t.Fatalf("error = %v, want command output", err)
	}
}

func TestDarwinRouteValidationAndDeleteCommandOutput(t *testing.T) {
	for _, network := range []string{"2001:db8::/32", "10.0.0.1/8"} {
		called := false
		err := addDarwinRoute(network, "utun2", func(string, ...string) ([]byte, error) {
			called = true
			return nil, nil
		})
		if err == nil || called {
			t.Fatalf("add %q error = %v, command called = %t; want validation failure without command", network, err, called)
		}
	}
	err := deleteDarwinRoute("10.0.0.0/8", "utun2", func(name string, args ...string) ([]byte, error) {
		want := []string{"-n", "delete", "-net", "10.0.0.0/8", "-interface", "utun2"}
		if name != "route" || !reflect.DeepEqual(args, want) {
			t.Errorf("command = %q %q, want %q %q", name, args, "route", want)
		}
		return []byte("not in table"), errors.New("exit status 1")
	})
	if err == nil || !strings.Contains(err.Error(), "not in table") {
		t.Fatalf("delete error = %v, want command output", err)
	}
}
