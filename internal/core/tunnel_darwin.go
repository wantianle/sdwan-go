//go:build darwin

package core

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/songgao/water"
)

// CreateTUN creates a TUN interface on macOS using the native utun kernel driver.
// No external driver needed — utun is part of the Darwin kernel.
func CreateTUN(name string, mtu int, _ string) (*water.Interface, error) {
	config := water.Config{
		DeviceType: water.TUN,
	}
	config.Name = name

	iface, err := water.New(config)
	if err != nil {
		return nil, fmt.Errorf("create utun: %w", err)
	}

	return iface, nil
}

// SetTUNIP assigns an IP address to the utun interface and brings it up.
// ip is in CIDR format, e.g. "10.100.100.7/24".
func SetTUNIP(name, ip, gateway string) error {
	// Parse CIDR: "10.100.100.7/24" -> IP + netmask
	parts := strings.SplitN(ip, "/", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid CIDR: %s", ip)
	}
	ipAddr := parts[0]

	cmd := exec.Command("ifconfig", name, "inet", ipAddr, ipAddr,
		"netmask", "255.255.255.0", "up")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("set IP on %s: %w", name, err)
	}
	return nil
}

// AddRoute adds a static route via the utun interface.
// gateway ignored on macOS (route goes through interface, not gateway).
func AddRoute(network, devName, _ string) error {
	return exec.Command("route", "-n", "add", "-net", network, "-interface", devName).Run()
}

// DelRoute removes a static route.
func DelRoute(network, devName, _ string) {
	exec.Command("route", "-n", "delete", "-net", network, "-interface", devName).Run()
}

// CloseTUN closes the utun interface. macOS automatically cleans up
// the virtual interface when closed.
func CloseTUN(iface TunDevice, devName string) {
	if iface != nil {
		iface.Close()
	}
}
