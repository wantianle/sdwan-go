package core

import (
	"net"
	"net/netip"
)

// RouteConflict represents an overlap between a configured SDWAN route
// and a local physical network interface subnet.
type RouteConflict struct {
	Interface string `json:"interface"`
	LocalCIDR string `json:"local_cidr"`
	RouteNet  string `json:"route_net"`
}

// detectRouteConflicts checks whether the configured routenet overlaps
// with any active local network interface subnet (excluding loopback and
// the TUN interface itself). Returns nil if no conflicts found.
func detectRouteConflicts(routeNet, tunName string) []RouteConflict {
	routePrefix, err := netip.ParsePrefix(routeNet)
	if err != nil || !routePrefix.Addr().Is4() {
		return nil
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	var conflicts []RouteConflict
	for _, iface := range ifaces {
		// Skip loopback, down interfaces, and the TUN adapter itself
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		if iface.Name == tunName {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue // skip IPv6 for now
			}

			ones, _ := ipnet.Mask.Size()
			ip4 := ipnet.IP.To4()
			if ip4 == nil {
				continue
			}
			localAddr := netip.AddrFrom4([4]byte{ip4[0], ip4[1], ip4[2], ip4[3]})
			localPrefix := netip.PrefixFrom(localAddr, ones)

			// Check if local CIDR overlaps with SDWAN route
			if routePrefix.Overlaps(localPrefix) {
				conflicts = append(conflicts, RouteConflict{
					Interface: iface.Name,
					LocalCIDR: ipnet.String(),
					RouteNet:  routeNet,
				})
				break // one overlap per interface is enough
			}
		}
	}
	return conflicts
}
