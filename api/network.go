package api

import (
	"net/netip"
	"strconv"
)

func (a ExternalAttachment) Key() string {
	vlan := 0
	if a.Vlan != nil {
		vlan = *a.Vlan
	}
	return a.NodeId + "/" + a.Interface + "/" + strconv.Itoa(vlan)
}

// RouterAddress is the product router, distinct from an existing LAN's gateway.
func (n Network) RouterAddress() string {
	if n.External != nil {
		if n.AllocationPool == nil {
			return ""
		}
		prefix, err := netip.ParsePrefix(*n.AllocationPool)
		if err != nil {
			return ""
		}
		return prefix.Masked().Addr().Next().String()
	}
	if n.Gateway != nil {
		return *n.Gateway
	}
	return ""
}
