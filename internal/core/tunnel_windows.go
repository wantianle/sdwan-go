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
// On server switch (panel restart), the adapter should persist so the new
// process can reuse it seamlessly. Only delete+recreate if the existing
// adapter is in a bad state (crash residue) and CreateTUN fails.
func CreateTUN(name string, mtu int, _ string) (TunDevice, error) {
	log.Printf("[WINTUN] Creating adapter name=%q mtu=%d", name, mtu)
	log.Printf("[WINTUN] Note: wintun adapter may not be visible in ncpa.cpl; use Device Manager or 'wmic nic get Name,Index' to verify")

	// Try to reuse the existing adapter first — this preserves the adapter
	// across server switches so we don't delete/recreate it unnecessarily.
	dev, err := tun.CreateTUN(name, mtu)
	if err != nil {
		log.Printf("[WINTUN] Reuse failed (%v), cleaning up stale adapter and retrying...", err)
		// Adapter likely in a bad state from a crash; delete it and retry.
		if out, errDel := exec.Command("wmic", "path", "Win32_NetworkAdapter",
			"where", fmt.Sprintf("NetConnectionID='%s'", name), "delete").CombinedOutput(); errDel == nil {
			log.Printf("[WINTUN] Cleaned up old adapter %q, retrying CreateTUN", name)
		} else {
			log.Printf("[WINTUN] Cleanup attempt: %s", string(out))
		}

		dev, err = tun.CreateTUN(name, mtu)
		if err != nil {
			log.Printf("[WINTUN] FAILED after cleanup: %v", err)
			log.Printf("[WINTUN] Common causes: (1) not run as Administrator, (2) wintun.dll not in same dir as exe, (3) driver blocked by antivirus")
			return nil, fmt.Errorf("create wintun adapter: %w", err)
		}
	}

	// Give Windows a moment to register the adapter in the IP stack.
	// This also helps when reusing — the driver may need a tick to settle.
	time.Sleep(500 * time.Millisecond)

	ifaceName, _ := dev.Name()
	log.Printf("[WINTUN] Adapter ready, name=%q", ifaceName)
	return &wintunDev{dev: dev, name: ifaceName}, nil
}

// SetTUNIP assigns a static IP to the TUN adapter via netsh.
// Gateway is set to the first host on the same subnet (e.g. 10.100.100.1)
// so Windows treats the interface as having a valid next-hop — without this
// the on-link route may not forward traffic. The dummy gateway is on the
// same /24 so it will NOT create a default route that steals internet traffic.
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
	return nil
}

// getTunIndex finds the Windows interface index for the given adapter name.
func getTunIndex(ifaceName string) (string, error) {
	log.Printf("[WINTUN] Looking up interface index for %q", ifaceName)
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
				log.Printf("[WINTUN] Found interface index=%s via %s", idx, field)
				return idx, nil
			}
		}
	}
	// Dump all NICs for diagnosis
	out, _ := exec.Command("wmic", "nic", "get", "Index,Name,NetConnectionID").Output()
	log.Printf("[WINTUN] Available NICs:\n%s", string(out))
	return "", fmt.Errorf("tun interface %q not found via wmic", ifaceName)
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
