//go:build windows

package core

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"golang.zx2c4.com/wireguard/tun"
)

// wintunDev wraps tun.Device from WireGuard to expose simple Read/Write.
// The underlying wintun.dll driver is a native Layer-3 TUN — Read/Write
// operate on raw IP packets, identical to Linux/macOS TUN.
type wintunDev struct {
	dev  tun.Device
	name string
}

func (d *wintunDev) Read(buf []byte) (int, error) {
	bufs := [][]byte{buf}
	sizes := []int{0}
	_, err := d.dev.Read(bufs, sizes, 0)
	return sizes[0], err
}

func (d *wintunDev) Write(buf []byte) (int, error) {
	bufs := [][]byte{buf}
	_, err := d.dev.Write(bufs, 0)
	return len(buf), err
}

func (d *wintunDev) Name() string { return d.name }
func (d *wintunDev) Close() error { return d.dev.Close() }

// CreateTUN creates a wintun TUN adapter (Layer 3, reads/writes IP packets).
// localCIDR is accepted for cross-platform signature compatibility; unused here
// because wintun does not need IP pre-configuration like tap0901 TUN mode does.
//
// IMPORTANT: tun.CreateTUN from wireguard-go/wintun always creates a NEW
// adapter instance — it never reopens an existing one. If an old adapter
// with the same name already exists, CreateTUN will create another with a
// suffixed name (e.g. "iwan1 #2"), leaving the old one orphaned in Device
// Manager. Therefore we must ALWAYS delete the old adapter first.
func CreateTUN(name string, mtu int, _ string) (TunDevice, error) {
	log.Printf("[WINTUN] Creating adapter name=%q mtu=%d", name, mtu)
	log.Printf("[WINTUN] Note: wintun adapter may not be visible in ncpa.cpl; use Device Manager or 'wmic nic get Name,Index' to verify")

	// --- Always clean up any existing adapter first ---
	// Errors are ignored — the adapter may not exist on first run.

	// Release IP binding (ignore errors)
	exec.Command("netsh", "interface", "ip", "set", "address",
		name, "dhcp").Run()

	// Force-disable the interface to release driver handles
	exec.Command("netsh", "interface", "set", "interface",
		name, "admin=disable").Run()

	// Delete all matching adapters (exact + suffixed like "iwan1 #2")
	if out, err := exec.Command("wmic", "path", "Win32_NetworkAdapter",
		"where", fmt.Sprintf("NetConnectionID like '%s%%'", name), "delete").CombinedOutput(); err == nil {
		log.Printf("[WINTUN] Cleaned up old adapter(s) matching %q", name)
	} else {
		log.Printf("[WINTUN] No old adapter to clean (or cleanup skipped): %s", string(out))
	}

	// Give the system a moment to actually release the adapter name
	time.Sleep(500 * time.Millisecond)

	// --- Create the new adapter ---
	dev, err := tun.CreateTUN(name, mtu)
	if err != nil {
		log.Printf("[WINTUN] FAILED: %v", err)
		log.Printf("[WINTUN] Common causes: (1) not run as Administrator, (2) wintun.dll not in same dir as exe, (3) driver blocked by antivirus")
		return nil, fmt.Errorf("create wintun adapter: %w", err)
	}

	// Give Windows a moment to register the new adapter in the IP stack
	time.Sleep(500 * time.Millisecond)

	ifaceName, _ := dev.Name()
	log.Printf("[WINTUN] Adapter created, name=%q", ifaceName)
	return &wintunDev{dev: dev, name: ifaceName}, nil
}

// SetTUNIP assigns a static IP to the TUN adapter via netsh.
// Gateway is set to the first host on the same subnet (e.g. 10.100.100.1)
// so Windows treats the interface as having a valid next-hop — without this
// the on-link route may not forward traffic. Windows may still create a
// default route/DNS side effects for that gateway, so SetTUNIP immediately
// cleans those up and leaves only the explicit SDWAN route added by AddRoute.
func SetTUNIP(name, ip, gateway string) error {
	// Derive a dummy gateway from the local IP: first host on the same /24
	lastDot := strings.LastIndex(ip, ".")
	dummyGW := ip[:lastDot+1] + "1"

	log.Printf("[WINTUN] Setting IP via netsh: name=%q ip=%s gw=%s (server=%s)", name, ip, dummyGW, gateway)
	out, err := exec.Command("netsh", "interface", "ip", "set", "address",
		name, "static", ip, "255.255.255.0", dummyGW, "1").CombinedOutput()
	if err != nil {
		log.Printf("[WINTUN] netsh failed: %s", string(out))
		return fmt.Errorf("netsh: %s", string(out))
	}
	log.Printf("[WINTUN] netsh OK")
	cleanupWindowsTunRouting(name, dummyGW)
	return nil
}

func cleanupWindowsTunRouting(name, dummyGW string) {
	log.Printf("[WINTUN] Hardening interface %q: metric=9999, remove default routes, clear DNS", name)
	runWindowsCleanup("set IPv4 metric", "netsh", "interface", "ipv4", "set", "interface", name, "metric=9999")
	runWindowsCleanup("set IPv6 metric", "netsh", "interface", "ipv6", "set", "interface", name, "metric=9999")
	runWindowsCleanup("clear IPv4 DNS", "netsh", "interface", "ipv4", "delete", "dnsservers", name, "all")
	runWindowsCleanup("clear IPv6 DNS", "netsh", "interface", "ipv6", "delete", "dnsservers", name, "all")

	idx, err := getTunIndex(name)
	if err != nil {
		log.Printf("[WINTUN] default route cleanup skipped interface-scoped delete: %v", err)
		runWindowsCleanup("delete default route", "route", "delete", "0.0.0.0", "mask", "0.0.0.0", dummyGW)
		runWindowsCleanup("delete persistent default route", "route", "-p", "delete", "0.0.0.0", "mask", "0.0.0.0", dummyGW)
		return
	}

	runWindowsCleanup("delete default route", "route", "delete", "0.0.0.0", "mask", "0.0.0.0", dummyGW, "IF", idx)
	runWindowsCleanup("delete persistent default route", "route", "-p", "delete", "0.0.0.0", "mask", "0.0.0.0", dummyGW, "IF", idx)
}

func runWindowsCleanup(label, name string, args ...string) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[WINTUN] cleanup warning (%s): %v %s", label, err, strings.TrimSpace(string(out)))
		return
	}
	log.Printf("[WINTUN] cleanup OK: %s", label)
}

// getTunIndex finds the Windows interface index for the given adapter name.
func getTunIndex(ifaceName string) (string, error) {
	log.Printf("[WINTUN] Looking up interface index for %q", ifaceName)

	// Prefer the IPv4 interface Idx from netsh. This is the index expected by
	// route.exe "IF <idx>" and may differ from Win32_NetworkAdapter.Index.
	if out, err := exec.Command("netsh", "interface", "ipv4", "show", "interfaces").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(strings.TrimSpace(line))
			if len(fields) < 5 || !allDigits(fields[0]) {
				continue
			}
			name := strings.Join(fields[4:], " ")
			if strings.EqualFold(strings.TrimSpace(name), ifaceName) {
				log.Printf("[WINTUN] Found interface index=%s via netsh ipv4", fields[0])
				return fields[0], nil
			}
		}
		log.Printf("[WINTUN] netsh ipv4 interface list did not contain %q", ifaceName)
	} else {
		log.Printf("[WINTUN] netsh ipv4 interface query failed: %v", err)
	}

	// Try NetConnectionID (the internal adapter name)
	for _, field := range []string{"NetConnectionID", "Name"} {
		out, err := exec.Command("wmic", "nic",
			"where", fmt.Sprintf("%s='%s'", field, ifaceName),
			"get", "Index").Output()
		if err != nil {
			log.Printf("[WINTUN] wmic %s query failed: %v", field, err)
			continue
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) >= 2 {
			idx := strings.TrimSpace(lines[1])
			if idx != "" {
				log.Printf("[WINTUN] Found fallback adapter index=%s via wmic %s (may differ from route IF index)", idx, field)
				return idx, nil
			}
		}
	}
	// Dump all NICs for diagnosis
	out, _ := exec.Command("wmic", "nic", "get", "Index,Name,NetConnectionID").Output()
	log.Printf("[WINTUN] Available NICs:\n%s", string(out))
	return "", fmt.Errorf("tun interface %q not found via wmic", ifaceName)
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// AddRoute adds an on-link route (gateway 0.0.0.0) through the TUN interface.
// On-link routing works because wintun is a proper L3 TUN: no ARP needed.
func AddRoute(_ string, tunName, _ string) error {
	idx, err := getTunIndex(tunName)
	if err != nil {
		return fmt.Errorf("getTunIndex: %w", err)
	}
	cmd := exec.Command("route", "add", "192.168.0.0", "mask", "255.255.0.0",
		"0.0.0.0", "IF", idx)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("route add IF=%s: %s", idx, string(out))
	}
	return nil
}

// DelRoute removes the tunnel route.
func DelRoute(_ string, tunName, _ string) {
	idx, err := getTunIndex(tunName)
	if err == nil {
		exec.Command("route", "delete", "192.168.0.0",
			"mask", "255.255.0.0", "0.0.0.0", "IF", idx).Run()
	} else {
		// Fallback: try without IF if interface lookup failed
		exec.Command("route", "delete", "192.168.0.0",
			"mask", "255.255.0.0", "0.0.0.0").Run()
	}
}

// CloseTUN shuts down the TUN adapter.
func CloseTUN(iface TunDevice, _ string) {
	if iface != nil {
		iface.Close()
	}
}
