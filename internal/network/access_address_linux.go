//go:build linux

package network

import (
	"fmt"
	"net"
	"net/netip"
	"slices"

	"github.com/vishvananda/netlink"
)

func AccessAddress(configured string) (string, error) {
	if configured != "" {
		address, err := netip.ParseAddr(configured)
		if err != nil {
			return "", fmt.Errorf("advertise address: %w", err)
		}
		return address.String(), nil
	}
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		routes, err := netlink.RouteList(nil, family)
		if err != nil {
			return "", err
		}
		slices.SortFunc(routes, func(a, b netlink.Route) int { return a.Priority - b.Priority })
		for _, route := range routes {
			if route.Dst != nil {
				ones, _ := route.Dst.Mask.Size()
				if ones != 0 {
					continue
				}
			}
			if route.LinkIndex == 0 {
				continue
			}
			probe := net.ParseIP("1.1.1.1")
			if family == netlink.FAMILY_V6 {
				probe = net.ParseIP("2606:4700:4700::1111")
			}
			resolved, err := netlink.RouteGet(probe)
			if err != nil {
				return "", err
			}
			for _, current := range resolved {
				if current.Src != nil {
					return current.Src.String(), nil
				}
			}
		}
	}
	return "", fmt.Errorf("node has no default route source; set advertise-address")
}
