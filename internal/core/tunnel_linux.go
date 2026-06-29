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

	err = exec.Command("ip", "link", "set", "dev", name, "mtu", fmt.Sprintf("%d", mtu)).Run()
	if err != nil {
		return nil, fmt.Errorf("set MTU on %s: %w", name, err)
	}

	return iface, nil
}

// SetTUNIP assigns an IP address (CIDR format, e.g. "10.100.100.7/24")
// and brings the TUN interface up.
func SetTUNIP(name, ip, gateway string) error {
	err := exec.Command("ip", "addr", "add", ip, "dev", name).Run()
	if err != nil {
		return fmt.Errorf("set IP on %s: %w", name, err)
	}
	err = exec.Command("ip", "link", "set", "dev", name, "up").Run()
	if err != nil {
		return fmt.Errorf("bring up %s: %w", name, err)
	}
	return nil
}

// AddRoute adds a static route through the TUN device.
// gateway is passed for cross-platform compatibility; ignored on Linux.
func AddRoute(network, devName, gateway string) error {
	return exec.Command("ip", "route", "add", network, "dev", devName, "metric", "10").Run()
}

// DelRoute removes a static route.
func DelRoute(network, devName, gateway string) {
	exec.Command("ip", "route", "del", network, "dev", devName, "metric", "10").Run()
}

// CloseTUN closes and destroys the TUN interface.
func CloseTUN(iface TunDevice, devName string) {
	if iface != nil {
		iface.Close()
	}
	exec.Command("ip", "link", "delete", devName).Run()
	syscall.Sync()
}
