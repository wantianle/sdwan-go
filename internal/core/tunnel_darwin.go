//go:build darwin

package core

import (
	"fmt"
	"log"
	"net"
	"net/netip"
	"os/exec"

	"github.com/songgao/water"
)

// darwinCommandRunner is intentionally local to the Darwin implementation so
// command invocations can be tested without executing privileged commands.
type darwinCommandRunner func(name string, args ...string) ([]byte, error)

func runDarwinCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// darwinTUNIPArgs validates point-to-point IPv4 inputs and builds the
// corresponding ifconfig arguments.
func darwinTUNIPArgs(local, peer string) ([]string, error) {
	localAddr, err := netip.ParseAddr(local)
	if err != nil || !localAddr.Is4() || localAddr.IsUnspecified() {
		return nil, fmt.Errorf("invalid local IPv4 address: %s", local)
	}
	peerAddr, err := netip.ParseAddr(peer)
	if err != nil || !peerAddr.Is4() || peerAddr.IsUnspecified() {
		return nil, fmt.Errorf("invalid peer IPv4 address: %s", peer)
	}

	return []string{"inet", localAddr.String(), peerAddr.String(), "netmask", darwinIPv4Netmask(), "up"}, nil
}

func darwinIPv4Netmask() string {
	return net.IP(net.CIDRMask(protocolIPv4Prefix, 32)).String()
}

// darwinRouteAddArgs builds macOS's interface route invocation for Phase 1.
func darwinRouteAddArgs(network, devName string) []string {
	return []string{"-n", "add", "-net", network, "-interface", devName}
}

// CreateTUN creates a dynamically allocated native macOS utun interface.
// Configured names are deliberately ignored: the actual iface.Name() is
// authoritative on Darwin.
func CreateTUN(_ string, mtu int, _ string) (*water.Interface, error) {
	config := water.Config{
		DeviceType: water.TUN,
	}
	iface, err := water.New(config)
	if err != nil {
		return nil, fmt.Errorf("create utun: %w", err)
	}

	// Apply the requested MTU to the actual utun interface.
	if err := setDarwinMTU(iface.Name(), mtu, runDarwinCommand); err != nil {
		iface.Close()
		return nil, err
	}

	return iface, nil
}

func setDarwinMTU(name string, mtu int, run darwinCommandRunner) error {
	out, err := run("ifconfig", name, "mtu", fmt.Sprintf("%d", mtu))
	if err != nil {
		return fmt.Errorf("set MTU %d on %s: %w (output: %s)", mtu, name, err, string(out))
	}
	return nil
}

// SetTUNIP assigns a protocol-compatible IPv4 address and point-to-point peer
// to the utun interface, then brings it up.
func SetTUNIP(name, ip, gateway string) error {
	return setDarwinTUNIP(name, ip, gateway, runDarwinCommand)
}

func setDarwinTUNIP(name, ip, gateway string, run darwinCommandRunner) error {
	prefix, err := netip.ParsePrefix(ip)
	if err != nil || !prefix.Addr().Is4() || prefix.Addr().IsUnspecified() || prefix.Bits() != protocolIPv4Prefix {
		return fmt.Errorf("invalid local IPv4 CIDR: %s", ip)
	}
	args, err := darwinTUNIPArgs(prefix.Addr().String(), gateway)
	if err != nil {
		return err
	}
	out, err := run("ifconfig", append([]string{name}, args...)...)
	if err != nil {
		return fmt.Errorf("set IP on %s: %w (output: %s)", name, err, string(out))
	}
	return nil
}

// AddRoute adds a static route via the utun interface.
// gateway ignored on macOS (route goes through interface, not gateway).
func AddRoute(network, devName, _ string) error {
	return addDarwinRoute(network, devName, runDarwinCommand)
}

func addDarwinRoute(network, devName string, run darwinCommandRunner) error {
	if _, err := darwinIPv4Route(network); err != nil {
		return err
	}
	out, err := run("route", darwinRouteAddArgs(network, devName)...)
	if err != nil {
		return fmt.Errorf("add route %s: %w (output: %s)", network, err, string(out))
	}
	return nil
}

func darwinIPv4Route(network string) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(network)
	if err != nil || !prefix.Addr().Is4() || prefix != prefix.Masked() {
		return netip.Prefix{}, fmt.Errorf("invalid IPv4 route network: %s", network)
	}
	return prefix, nil
}

func deleteDarwinRoute(network, devName string, run darwinCommandRunner) error {
	if _, err := darwinIPv4Route(network); err != nil {
		return err
	}
	out, err := run("route", "-n", "delete", "-net", network, "-interface", devName)
	if err != nil {
		return fmt.Errorf("delete route %s: %w (output: %s)", network, err, string(out))
	}
	return nil
}

// DelRoute removes a static route. Cleanup is best-effort, but failures retain
// command diagnostics in the process log.
func DelRoute(network, devName, _ string) {
	if err := deleteDarwinRoute(network, devName, runDarwinCommand); err != nil {
		log.Printf("[WARN] Delete route failed: %v", err)
	}
}

// CloseTUN closes the utun interface. macOS automatically cleans up
// the virtual interface when closed.
func CloseTUN(iface TunDevice, devName string) {
	if iface != nil {
		iface.Close()
	}
}
