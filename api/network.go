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

// WithRouting expands the default topology accepted by the public API.
func (s EnvironmentSpec) WithRouting() EnvironmentSpec {
	if s.Routers != nil {
		return s
	}
	ids := make(map[string]bool, len(s.Networks)+len(s.Assets))
	for _, network := range s.Networks {
		ids[network.Id] = true
		for _, device := range network.Devices {
			ids[device.Id] = true
		}
	}
	for _, asset := range s.Assets {
		ids[asset.Id] = true
	}
	interfaceIDs := map[string]bool{}
	for _, asset := range s.Assets {
		for _, iface := range asset.Interfaces {
			interfaceIDs[iface.Id] = true
		}
	}
	id := "gateway"
	for ids[id] {
		id += "_"
	}
	router := LogicalRouter{Id: id, Name: "默认路由器", Interfaces: []RouterInterface{}}
	for _, network := range s.Networks {
		address := network.RouterAddress()
		if address == "" && network.External == nil {
			if prefix, err := netip.ParsePrefix(network.Cidr); err == nil {
				address = prefix.Masked().Addr().Next().String()
			}
		}
		if address != "" {
			id := network.Id
			for interfaceIDs[id] {
				id += "_"
			}
			interfaceIDs[id] = true
			router.Interfaces = append(router.Interfaces, RouterInterface{Id: id, NetworkId: network.Id, Address: address})
		}
	}
	routers := []LogicalRouter{}
	if len(router.Interfaces) > 0 {
		routers = append(routers, router)
	}
	s.Routers = &routers
	if s.Routes != nil {
		routes := append([]Route{}, (*s.Routes)...)
		for i := range routes {
			if routes[i].RouterId == nil {
				id := router.Id
				routes[i].RouterId = &id
			}
		}
		s.Routes = &routes
	}
	return s
}
