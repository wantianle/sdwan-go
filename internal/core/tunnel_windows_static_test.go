package core

import (
	"os"
	"strings"
	"testing"
)

func TestWindowsCreateTUNDoesNotRunDestructivePreCleanup(t *testing.T) {
	source, err := os.ReadFile("tunnel_windows.go")
	if err != nil {
		t.Fatalf("read Windows TUN source: %v", err)
	}

	createTUN := string(source)
	start := strings.Index(createTUN, "func CreateTUN(")
	end := strings.Index(createTUN, "\n// SetTUNIP")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("could not locate CreateTUN source")
	}
	createTUN = createTUN[start:end]

	for _, forbidden := range []string{
		"exec.Command(",
		"netsh",
		"wmic",
		"dhcp",
		"admin=disable",
		"Win32_NetworkAdapter",
	} {
		if strings.Contains(strings.ToLower(createTUN), strings.ToLower(forbidden)) {
			t.Errorf("CreateTUN must not perform destructive pre-cleanup: found %q", forbidden)
		}
	}

	if !strings.Contains(createTUN, "tun.CreateTUN(name, mtu)") {
		t.Error("CreateTUN must create or reuse the requested Wintun adapter")
	}
}

func TestWindowsRoutesRequireAuthoritativeIPv4InterfaceIndex(t *testing.T) {
	source, err := os.ReadFile("tunnel_windows.go")
	if err != nil {
		t.Fatalf("read Windows TUN source: %v", err)
	}

	text := string(source)
	getIndex := sourceFunction(t, text, "getTunIndex", "validWindowsInterfaceIndex")
	for _, forbidden := range []string{"wmic", "Win32_NetworkAdapter", "NetConnectionID"} {
		if strings.Contains(getIndex, forbidden) {
			t.Errorf("Windows route index lookup must not use fallback %q", forbidden)
		}
	}
	if !strings.Contains(getIndex, `"netsh", "interface", "ipv4", "show", "interfaces"`) {
		t.Error("getTunIndex must query the route-compatible IPv4 interface list")
	}
	if !strings.Contains(getIndex, "route-compatible IPv4 interface index") {
		t.Error("getTunIndex must fail rather than return a non-route-compatible index")
	}

	addRoute := sourceFunction(t, text, "AddRoute", "DelRoute")
	if !strings.Contains(addRoute, "return fmt.Errorf") || !strings.Contains(addRoute, "authoritative IPv4 interface index") {
		t.Error("AddRoute must return an error when authoritative index lookup fails")
	}
}

func TestWindowsRouteDeletionIsAlwaysInterfaceScoped(t *testing.T) {
	source, err := os.ReadFile("tunnel_windows.go")
	if err != nil {
		t.Fatalf("read Windows TUN source: %v", err)
	}

	text := string(source)
	cleanup := sourceFunction(t, text, "cleanupWindowsTunRouting", "runWindowsCleanup")
	if strings.Contains(cleanup, `"0.0.0.0", "mask", "0.0.0.0", dummyGW)`) {
		t.Error("default-route cleanup must not issue an unscoped route delete")
	}
	if !strings.Contains(cleanup, `dummyGW, "IF", idx)`) {
		t.Error("default-route cleanup must remain interface scoped")
	}

	delRoute := sourceFunction(t, text, "DelRoute", "CloseTUN")
	if strings.Contains(delRoute, `"0.0.0.0").`) {
		t.Error("DelRoute must not fall back to an unscoped route delete")
	}
	if !strings.Contains(delRoute, `"0.0.0.0", "IF", idx`) {
		t.Error("DelRoute must delete only through an explicit interface index")
	}
	if !strings.Contains(delRoute, "rememberedWindowsRouteIndex") {
		t.Error("DelRoute must require the retained successful AddRoute ownership index")
	}
	if strings.Contains(delRoute, "getTunIndex") {
		t.Error("DelRoute must not re-resolve an interface index when route ownership is missing")
	}
	if !strings.Contains(delRoute, "cleanup incomplete") || !strings.Contains(delRoute, "ownership index is missing") {
		t.Error("DelRoute must log incomplete cleanup when retained route ownership is missing")
	}
}

func sourceFunction(t *testing.T, source, startMarker, endMarker string) string {
	t.Helper()
	start := strings.Index(source, "func "+startMarker+"(")
	end := strings.Index(source, "func "+endMarker+"(")
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("could not locate %s source", startMarker)
	}
	return source[start:end]
}
