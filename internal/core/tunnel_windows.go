//go:build windows

package core

import (
	"fmt"
	"log"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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

type windowsRouteKey struct {
	tunName string
	target  string
	mask    string
}

var windowsRouteIndexes = struct {
	sync.Mutex
	byRoute map[windowsRouteKey]string
}{byRoute: make(map[windowsRouteKey]string)}

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

// CreateTUN creates or reopens a named Wintun adapter (Layer 3, reads/writes
// IP packets). The pinned wireguard/tun implementation reuses an existing
// adapter with the requested name. localCIDR is accepted for cross-platform
// signature compatibility; it is unused here because Wintun does not need IP
// pre-configuration like tap0901 TUN mode does.
func CreateTUN(name string, mtu int, _ string) (TunDevice, error) {
	log.Printf("[WINTUN] Creating adapter name=%q mtu=%d", name, mtu)
	dev, err := tun.CreateTUN(name, mtu)
	if err != nil {
		log.Printf("[WINTUN] FAILED: %v", err)
		log.Printf("[WINTUN] Common causes: (1) not run as Administrator, (2) wintun.dll not in same dir as exe, (3) driver blocked by antivirus")
		return nil, fmt.Errorf("create wintun adapter: %w", err)
	}

	// Wait for the created or reopened adapter to register in the IP stack
	// before the caller configures it. This is a registration settle, not an
	// adapter-deletion delay.
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
	// ip may be CIDR e.g. "10.100.100.7/24"; netsh expects bare IP.
	bareIP := strings.SplitN(ip, "/", 2)[0]

	// Derive a dummy gateway from the local IP: first host on the same /24
	lastDot := strings.LastIndex(bareIP, ".")
	dummyGW := bareIP[:lastDot+1] + "1"

	log.Printf("[WINTUN] Setting IP via netsh: name=%q ip=%s gw=%s (server=%s)", name, bareIP, dummyGW, gateway)
	out, err := exec.Command("netsh", "interface", "ip", "set", "address",
		name, "static", bareIP, "255.255.255.0", dummyGW, "1").CombinedOutput()
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
		log.Printf("[WINTUN] cleanup incomplete: default-route deletion skipped for interface=%q gateway=%s because authoritative IPv4 interface index is unavailable: %v", name, dummyGW, err)
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

// getTunIndex finds the route-compatible Windows IPv4 interface index for the
// given adapter name.
func getTunIndex(ifaceName string) (string, error) {
	log.Printf("[WINTUN] Looking up interface index for %q", ifaceName)

	// The IPv4 interface Idx from netsh is the index expected by route.exe
	// "IF <idx>".
	if out, err := exec.Command("netsh", "interface", "ipv4", "show", "interfaces").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(strings.TrimSpace(line))
			if len(fields) < 5 || !validWindowsInterfaceIndex(fields[0]) {
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

	return "", fmt.Errorf("route-compatible IPv4 interface index for %q is unavailable", ifaceName)
}

func validWindowsInterfaceIndex(s string) bool {
	idx, err := strconv.ParseUint(s, 10, 32)
	return err == nil && idx > 0
}

func windowsRouteTarget(network string) (string, string, error) {
	ip, cidr, err := net.ParseCIDR(network)
	if err != nil {
		return "", "", fmt.Errorf("parse network CIDR %q: %w", network, err)
	}
	if ip.To4() == nil || len(cidr.Mask) != net.IPv4len {
		return "", "", fmt.Errorf("route network %q must be IPv4", network)
	}
	canonicalIP := cidr.IP.To4()
	if canonicalIP == nil {
		return "", "", fmt.Errorf("route network %q has no canonical IPv4 network", network)
	}
	mask := cidr.Mask
	return canonicalIP.String(), fmt.Sprintf("%d.%d.%d.%d", mask[0], mask[1], mask[2], mask[3]), nil
}

func rememberWindowsRouteIndex(tunName, target, mask, idx string) {
	windowsRouteIndexes.Lock()
	defer windowsRouteIndexes.Unlock()
	windowsRouteIndexes.byRoute[windowsRouteKey{tunName: tunName, target: target, mask: mask}] = idx
}

func rememberedWindowsRouteIndex(tunName, target, mask string) (string, bool) {
	windowsRouteIndexes.Lock()
	defer windowsRouteIndexes.Unlock()
	idx, ok := windowsRouteIndexes.byRoute[windowsRouteKey{tunName: tunName, target: target, mask: mask}]
	return idx, ok
}

func forgetWindowsRouteIndex(tunName, target, mask string) {
	windowsRouteIndexes.Lock()
	defer windowsRouteIndexes.Unlock()
	delete(windowsRouteIndexes.byRoute, windowsRouteKey{tunName: tunName, target: target, mask: mask})
}

// AddRoute adds an on-link route (gateway 0.0.0.0) through the TUN interface.
func AddRoute(network string, tunName, _ string) error {
	ip, mask, err := windowsRouteTarget(network)
	if err != nil {
		return err
	}

	idx, err := getTunIndex(tunName)
	if err != nil {
		return fmt.Errorf("route add target=%s mask=%s: authoritative IPv4 interface index for %q: %w", ip, mask, tunName, err)
	}
	cmd := exec.Command("route", "add", ip, "mask", mask, "0.0.0.0", "IF", idx)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("route add target=%s mask=%s IF=%s: %s", ip, mask, idx, strings.TrimSpace(string(out)))
	}
	rememberWindowsRouteIndex(tunName, ip, mask, idx)
	return nil
}

// DelRoute removes the tunnel route.
func DelRoute(network string, tunName, _ string) {
	ip, mask, err := windowsRouteTarget(network)
	if err != nil {
		log.Printf("[DELROUTE] invalid route network=%q: %v", network, err)
		return
	}

	idx, ok := rememberedWindowsRouteIndex(tunName, ip, mask)
	if !ok {
		log.Printf("[DELROUTE] cleanup incomplete: delete skipped target=%s mask=%s alias=%q because retained route ownership index is missing", ip, mask, tunName)
		return
	}

	out, err := exec.Command("route", "delete", ip, "mask", mask, "0.0.0.0", "IF", idx).CombinedOutput()
	if err != nil {
		log.Printf("[DELROUTE] delete failed target=%s mask=%s IF=%s: %v output=%q", ip, mask, idx, err, strings.TrimSpace(string(out)))
		return
	}
	log.Printf("[DELROUTE] delete OK target=%s mask=%s IF=%s output=%q", ip, mask, idx, strings.TrimSpace(string(out)))
	forgetWindowsRouteIndex(tunName, ip, mask)
}

// CloseTUN releases the Wintun handle. It intentionally retains the named
// adapter so a later CreateTUN can reuse it.
func CloseTUN(iface TunDevice, _ string) {
	if iface != nil {
		iface.Close()
	}
}
