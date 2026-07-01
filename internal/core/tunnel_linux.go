//go:build linux

package core

import (
	"fmt"
	"os/exec"
	"syscall"

	"github.com/songgao/water"
)

// CreateTUN creates a TUN interface with the given name and MTU.
// On Linux this uses /dev/net/tun (kernel native).
func CreateTUN(name string, mtu int, _ string) (*water.Interface, error) {
	config := water.Config{
		DeviceType: water.TUN,
	}
	config.Name = name

	iface, err := water.New(config)
	if err != nil {
		return nil, fmt.Errorf("create TUN %s: %w", name, err)
	}

	out, err := exec.Command("ip", "link", "set", "dev", name, "mtu", fmt.Sprintf("%d", mtu)).CombinedOutput()
	if err != nil {
		iface.Close()
		exec.Command("ip", "link", "delete", name).Run()
		return nil, fmt.Errorf("set MTU on %s: %w (output: %s)", name, err, string(out))
	}

	return iface, nil
}

// SetTUNIP assigns an IP address (CIDR format, e.g. "10.100.100.7/24")
// and brings the TUN interface up.
func SetTUNIP(name, ip, gateway string) error {
	// Replacement-friendly: switch can reassign a new server-provided tunnel IP
	// on the same TUN. Ignore flush errors; there may be no address yet.
	_ = exec.Command("ip", "addr", "flush", "dev", name).Run()

	out, err := exec.Command("ip", "addr", "add", ip, "dev", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("set IP on %s: %w (output: %s)", name, err, string(out))
	}
	out, err = exec.Command("ip", "link", "set", "dev", name, "up").CombinedOutput()
	if err != nil {
		return fmt.Errorf("bring up %s: %w (output: %s)", name, err, string(out))
	}
	return nil
}

// AddRoute adds a static route through the TUN device.
// gateway is passed for cross-platform compatibility; ignored on Linux.
func AddRoute(network, devName, gateway string) error {
	out, err := exec.Command("ip", "route", "add", network, "dev", devName, "metric", "10").CombinedOutput()
	if err != nil {
		return fmt.Errorf("add route %s: %w (output: %s)", network, err, string(out))
	}
	return nil
}

// DelRoute removes a static route.
func DelRoute(network, devName, gateway string) {
	out, err := exec.Command("ip", "route", "del", network, "dev", devName, "metric", "10").CombinedOutput()
	if err != nil {
		// Route may already be gone; log but don't fail.
		_ = out
	}
}

// CloseTUN closes and destroys the TUN interface.
func CloseTUN(iface TunDevice, devName string) {
	if iface != nil {
		iface.Close()
	}
	exec.Command("ip", "link", "delete", devName).Run()
	syscall.Sync()
}
