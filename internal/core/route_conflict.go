package core

import "net"

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
	_, routeNetParsed, err := net.ParseCIDR(routeNet)
	if err != nil || routeNetParsed == nil || routeNetParsed.IP.To4() == nil {
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

			// Check if local CIDR overlaps with SDWAN route
			if cidrOverlap(ipnet, routeNetParsed) {
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

// cidrOverlap returns true if two IPv4 CIDR networks overlap.
func cidrOverlap(a, b *net.IPNet) bool {
	aFirst, aLast := cidrRange(a)
	bFirst, bLast := cidrRange(b)
	// Overlap if a starts before b ends AND b starts before a ends
	return !(aLast < bFirst || bLast < aFirst)
}

// cidrRange returns the first and last IP addresses of a CIDR as uint32.
func cidrRange(n *net.IPNet) (first, last uint32) {
	ip4 := n.IP.Mask(n.Mask).To4()
	if ip4 == nil {
		return 0, 0
	}
	first = uint32(ip4[0])<<24 | uint32(ip4[1])<<16 | uint32(ip4[2])<<8 | uint32(ip4[3])
	maskOnes, _ := n.Mask.Size()
	if maskOnes == 32 {
		return first, first
	}
	hostBits := 32 - maskOnes
	last = first | ((1 << hostBits) - 1)
	return
}
