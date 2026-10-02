package network

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/ovn-org/libovsdb/model"
	"netlab.local/core/api"
)

type vpnPeer struct {
	Peer      api.VPNPeer `json:"peer"`
	Addresses []string    `json:"addresses"`
}

type vpnRecord struct {
	PrivateKey string        `json:"privateKey"`
	Transit    []string      `json:"transit"`
	Clients    []string      `json:"clients"`
	Networks   []api.Network `json:"networks"`
	Peers      []vpnPeer     `json:"peers"`
	ListenPort int           `json:"listenPort"`
}

const vpnMTU = 1280

func vpnName(environment string) string { return "nlvpn-" + environment }
func vpnDevice(environment string) string {
	id := strings.ReplaceAll(environment, "-", "")
	return "nv" + id[len(id)-12:]
}
func vpnPort(environment string) string { return objectName("vpn", environment, "access") }

func freePrefix(bits int, pools []string, occupied []netip.Prefix) (netip.Prefix, error) {
	for _, value := range pools {
		pool := netip.MustParsePrefix(value)
		candidate := netip.PrefixFrom(pool.Addr(), bits)
		for candidate.IsValid() && pool.Contains(candidate.Addr()) {
			var collision netip.Prefix
			for _, used := range occupied {
				if candidate.Overlaps(used) {
					collision = used
					break
				}
			}
			if !collision.IsValid() {
				return candidate, nil
			}
			end := candidate
			if collision.Bits() < candidate.Bits() {
				end = collision.Masked()
			}
			address := end.Addr().AsSlice()
			for bit := end.Bits(); bit < end.Addr().BitLen(); bit++ {
				address[bit/8] |= 1 << (7 - bit%8)
			}
			last, _ := netip.AddrFromSlice(address)
			next := last.Next()
			if !next.IsValid() {
				break
			}
			candidate = netip.PrefixFrom(next, bits).Masked()
		}
	}
	return netip.Prefix{}, fmt.Errorf("no private VPN range outside environment and access networks")
}

func vpnRanges(plan api.NodePlan) ([]string, []string, error) {
	var used []netip.Prefix
	for _, network := range plan.Spec.Networks {
		prefix, err := netip.ParsePrefix(network.Cidr)
		if err != nil {
			return nil, nil, err
		}
		used = append(used, prefix)
	}
	for _, peer := range plan.Vpn.Peers {
		for _, route := range peer.Routes {
			prefix, err := netip.ParsePrefix(route.AccessCidr)
			if err != nil {
				return nil, nil, err
			}
			used = append(used, prefix)
		}
	}
	var transit, clients []string
	for _, family := range []struct {
		pools []string
		bits  int
	}{{[]string{"198.18.0.0/15", "172.16.0.0/12", "10.0.0.0/8"}, 20}, {[]string{"fdff::/16", "fdfe::/16", "fd00::/8"}, 64}} {
		client, err := freePrefix(family.bits, family.pools, used)
		if err != nil {
			return nil, nil, err
		}
		used = append(used, client)
		linkBits := 64
		if client.Addr().Is4() {
			linkBits = 30
		}
		link, err := freePrefix(linkBits, family.pools, used)
		if err != nil {
			return nil, nil, err
		}
		used = append(used, link)
		clients = append(clients, client.String())
		transit = append(transit, link.String())
	}
	return transit, clients, nil
}

func assignVPNPeers(record vpnRecord, peers []api.VPNPeer) ([]vpnPeer, error) {
	previous := map[string]vpnPeer{}
	used := map[string]bool{}
	for _, peer := range record.Peers {
		previous[peer.Peer.Id] = peer
	}
	result := make([]vpnPeer, 0, len(peers))
	for _, peer := range peers {
		item := previous[peer.Id]
		item.Peer = peer
		item.Peer.Addresses = nil
		if peer.Addresses != nil {
			item.Addresses = slices.Clone(*peer.Addresses)
		}
		for index, value := range item.Addresses {
			address, err := netip.ParsePrefix(value)
			if err != nil || address.Bits() != address.Addr().BitLen() {
				return nil, fmt.Errorf("VPN peer %s requires host addresses", peer.Id)
			}
			inPool := false
			for _, pool := range record.Clients {
				prefix := netip.MustParsePrefix(pool)
				if prefix.Contains(address.Addr()) && address.Addr() != prefix.Addr() && address.Addr() != prefix.Addr().Next() && (!address.Addr().Is4() || prefix.Contains(address.Addr().Next())) {
					inPool = true
				}
			}
			value = address.String()
			item.Addresses[index] = value
			if !inPool || used[value] {
				return nil, fmt.Errorf("VPN peer %s address %s is outside its client pools or already assigned", peer.Id, value)
			}
			used[value] = true
		}
		result = append(result, item)
	}
	for index := range result {
		item := &result[index]
		for _, value := range record.Clients {
			prefix := netip.MustParsePrefix(value)
			if slices.ContainsFunc(item.Addresses, func(value string) bool { return netip.MustParsePrefix(value).Addr().Is4() == prefix.Addr().Is4() }) {
				continue
			}
			address := prefix.Addr().Next().Next()
			for address.IsValid() && prefix.Contains(address) && used[netip.PrefixFrom(address, address.BitLen()).String()] {
				address = address.Next()
			}
			if !address.IsValid() || !prefix.Contains(address) || (address.Is4() && !prefix.Contains(address.Next())) {
				return nil, fmt.Errorf("VPN client address space exhausted")
			}
			cidr := netip.PrefixFrom(address, address.BitLen()).String()
			item.Addresses = append(item.Addresses, cidr)
			used[cidr] = true
		}
	}
	return result, nil
}

func vpnRangeConflict(record vpnRecord, plan api.NodePlan) error {
	for _, value := range append(slices.Clone(record.Transit), record.Clients...) {
		reserved := netip.MustParsePrefix(value)
		for _, network := range plan.Spec.Networks {
			if reserved.Overlaps(netip.MustParsePrefix(network.Cidr)) {
				return fmt.Errorf("network %s (%s) overlaps active VPN range %s", network.Name, network.Cidr, value)
			}
		}
		if plan.Vpn != nil {
			for _, peer := range plan.Vpn.Peers {
				for _, route := range peer.Routes {
					if reserved.Overlaps(netip.MustParsePrefix(route.AccessCidr)) {
						return fmt.Errorf("VPN access range %s overlaps client or transit range %s", route.AccessCidr, value)
					}
				}
			}
		}
	}
	return nil
}

func vpnModels(environment string, record *vpnRecord, router *Router, chassis string) []model.Model {
	if record == nil || len(record.Peers) == 0 {
		return nil
	}
	owner := func() map[string]string {
		ids := ownership(environment)
		ids["netlab.component"] = "vpn"
		return ids
	}
	rp := &RouterPort{UUID: "vpn_rp", Name: objectName("vpn_rp", environment, "access"), ExternalIDs: owner()}
	var addresses []string
	for _, value := range record.Transit {
		prefix := netip.MustParsePrefix(value)
		rp.Networks = append(rp.Networks, netip.PrefixFrom(prefix.Addr().Next(), prefix.Bits()).String())
		addresses = append(addresses, prefix.Addr().Next().Next().String())
	}
	rp.MAC = routerMAC(netip.MustParsePrefix(record.Transit[0]).Addr().Next())
	portMAC := routerMAC(netip.MustParsePrefix(record.Transit[0]).Addr().Next().Next())
	port := &SwitchPort{UUID: "vpn_port", Name: vpnPort(environment), Addresses: []string{portMAC + " " + strings.Join(addresses, " ")}, PortSecurity: []string{portMAC + " " + strings.Join(addresses, " ")}, ExternalIDs: owner()}
	connection := &SwitchPort{UUID: "vpn_connection", Name: objectName("vpn_sp", environment, "router"), Type: "router", Addresses: []string{"router"}, Options: map[string]string{"router-port": rp.Name}, ExternalIDs: owner()}
	sw := &Switch{UUID: "vpn_switch", Name: objectName("vpn_ls", environment, "access"), Ports: []string{port.UUID, connection.UUID}, ExternalIDs: owner()}
	router.Ports = append(router.Ports, rp.UUID)
	if router.Options == nil {
		router.Options = map[string]string{}
	}
	router.Options["chassis"] = chassis
	models := []model.Model{rp, port, connection, sw}
	// SNAT to each existing gateway also covers secondary guest NICs without a default route.
	for index, network := range record.Networks {
		prefix := netip.MustParsePrefix(network.Cidr)
		var source netip.Addr
		for _, value := range record.Transit {
			transit := netip.MustParsePrefix(value)
			if transit.Addr().Is4() == prefix.Addr().Is4() {
				source = transit.Addr().Next().Next()
			}
		}
		set := &AddressSet{UUID: fmt.Sprintf("vpn_targets%d", index), Name: strings.ReplaceAll(objectName("vpn_targets", environment, network.Id), "-", "_"), Addresses: []string{prefix.String()}, ExternalIDs: owner()}
		nat := &NAT{UUID: fmt.Sprintf("vpn_nat%d", index), Type: "snat", LogicalIP: source.String(), ExternalIP: *network.Gateway, AllowedExtIPs: &set.UUID, ExternalIDs: owner()}
		router.NAT = append(router.NAT, nat.UUID)
		models = append(models, set, nat)
	}
	return models
}

func removeVPNReferences(router *Router, ports []RouterPort, nats []NAT) {
	for _, port := range ports {
		router.Ports = slices.DeleteFunc(router.Ports, func(id string) bool { return id == port.UUID })
	}
	for _, nat := range nats {
		router.NAT = slices.DeleteFunc(router.NAT, func(id string) bool { return id == nat.UUID })
	}
}
