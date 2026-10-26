package network

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/ovn-org/libovsdb/model"
	"netlab.local/core/api"
)

type vpnPeer struct {
	Peer      api.VPNPeer `json:"peer"`
	Addresses []string    `json:"addresses"`
}

type accessRecord struct {
	Version    int           `json:"version"`
	PrivateKey string        `json:"privateKey"`
	Clients    []string      `json:"clients"`
	Networks   []api.Network `json:"networks"`
	Peers      []vpnPeer     `json:"peers"`
	ListenPort int           `json:"listenPort"`
}

type gatewayRecord struct {
	Gateway  api.ServiceGateway       `json:"gateway"`
	Services []api.NodeServiceBinding `json:"services"`
}

func accessLinkID(environment, network string) string {
	return strings.ReplaceAll(uuid.NewSHA1(uuid.NameSpaceOID, []byte(environment+"/"+network)).String(), "-", "")[:12]
}
func accessNetworkPort(environment, network string) string {
	return objectName("management", environment, network)
}

const vpnMTU = 1280

func accessName(environment string) string { return "nlaccess-" + environment }
func accessDevice(environment string) string {
	id := strings.ReplaceAll(environment, "-", "")
	return "nv" + id[len(id)-12:]
}
func accessPort(environment string) string { return objectName("access", environment, "access") }

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
	return netip.Prefix{}, fmt.Errorf("no private access range outside environment and VPN networks")
}

func vpnRanges(plan api.NodePlan) ([]string, error) {
	var used []netip.Prefix
	for _, network := range plan.Spec.Networks {
		prefix, err := netip.ParsePrefix(network.Cidr)
		if err != nil {
			return nil, err
		}
		used = append(used, prefix)
	}
	for _, peer := range plan.Vpn.Peers {
		for _, route := range peer.Routes {
			prefix, err := netip.ParsePrefix(route.AccessCidr)
			if err != nil {
				return nil, err
			}
			used = append(used, prefix)
		}
	}
	var clients []string
	for _, family := range []struct {
		pools []string
		bits  int
	}{{[]string{"198.18.0.0/15", "172.16.0.0/12", "10.0.0.0/8"}, 20}, {[]string{"fdff::/16", "fdfe::/16", "fd00::/8"}, 64}} {
		client, err := freePrefix(family.bits, family.pools, used)
		if err != nil {
			return nil, err
		}
		used = append(used, client)
		clients = append(clients, client.String())
	}
	return clients, nil
}

func assignVPNPeers(record accessRecord, peers []api.VPNPeer) ([]vpnPeer, error) {
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

func vpnRangeConflict(record accessRecord, plan api.NodePlan) error {
	for _, value := range record.Clients {
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

func accessModels(environment string, record *accessRecord, gateway *api.ServiceGateway, provider netip.Prefix) ([]model.Model, error) {
	if record == nil {
		return nil, nil
	}
	owner := func() map[string]string {
		ids := ownership(environment)
		ids["netlab.component"] = "access"
		return ids
	}
	var models []model.Model
	for index, network := range record.Networks {
		if network.AccessAddress == nil {
			continue
		}
		address, err := netip.ParseAddr(*network.AccessAddress)
		if err != nil {
			return nil, err
		}
		ids := owner()
		ids["netlab.network"] = network.Id
		security := routerMAC(address) + " " + address.String()
		models = append(models, &SwitchPort{UUID: fmt.Sprintf("management%d", index), Name: accessNetworkPort(environment, network.Id), Addresses: []string{security}, PortSecurity: []string{security}, ExternalIDs: ids})
	}
	if gateway != nil {
		address, err := netip.ParseAddr(gateway.Address)
		if err != nil || !provider.IsValid() || !provider.Contains(address) || address == provider.Addr().Next() {
			return nil, fmt.Errorf("environment gateway is outside the node service address pool")
		}
		for _, network := range record.Networks {
			if provider.Overlaps(netip.MustParsePrefix(network.Cidr)) {
				return nil, fmt.Errorf("network %s overlaps reserved service network %s", network.Name, provider)
			}
		}
		security := routerMAC(address) + " " + address.String()
		port := &SwitchPort{UUID: "access_provider", Name: accessPort(environment), Addresses: []string{security}, PortSecurity: []string{security}, ExternalIDs: owner()}
		local := &SwitchPort{UUID: "access_localnet", Name: objectName("access_localnet", environment, "provider"), Type: "localnet", Addresses: []string{"unknown"}, Options: map[string]string{"network_name": providerNetwork(gateway.NodeId)}, ExternalIDs: owner()}
		models = append(models, port, local, &Switch{UUID: "access_provider_switch", Name: objectName("access_ls", environment, "provider"), Ports: []string{port.UUID, local.UUID}, ExternalIDs: owner()})
	}
	return models, nil
}

func accessNetwork(record accessRecord, plan api.NodePlan) (accessRecord, error) {
	if err := vpnRangeConflict(record, plan); err != nil {
		if len(record.Peers) > 0 {
			return record, err
		}
		if plan.Vpn == nil {
			plan.Vpn = &api.NodeVPNPlan{}
		}
		var err error
		if record.Clients, err = vpnRanges(plan); err != nil {
			return record, err
		}
	}
	record.Networks = slices.Clone(plan.Spec.Networks)
	record.Version = 2
	return record, nil
}
