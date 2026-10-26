package environment

import (
	"net/netip"
	"reflect"
	"strings"

	"github.com/google/uuid"
	"netlab.local/core/api"
)

// The address is server-owned. Editing a network must not reallocate its
// management endpoint just because a client omitted read-only response fields.
func preserveNetworkAccess(before, after api.EnvironmentSpec) api.EnvironmentSpec {
	networks := make(map[string]api.Network, len(before.Networks))
	for _, network := range before.Networks {
		networks[network.Id] = network
	}
	for i := range after.Networks {
		network := &after.Networks[i]
		network.AccessAddress = nil
		if old, exists := networks[network.Id]; exists && old.Cidr == network.Cidr && reflect.DeepEqual(old.AllocationPool, network.AllocationPool) {
			network.AccessAddress = old.AccessAddress
		}
	}
	return after
}

func networkDesign(network api.Network) api.Network {
	network.AccessAddress = nil
	return network
}

func normalizeRouting(spec *api.EnvironmentSpec, networks map[string]api.Network, pools map[string]netip.Prefix, used map[string]map[netip.Addr]bool, objects map[string]bool) error {
	interfaces := map[string]bool{}
	explicitAssets := map[string]map[netip.Addr]bool{}
	for _, asset := range spec.Assets {
		for _, iface := range asset.Interfaces {
			if explicitAssets[iface.NetworkId] == nil {
				explicitAssets[iface.NetworkId] = map[netip.Addr]bool{}
			}
			if address, err := netip.ParseAddr(iface.Address); err == nil {
				explicitAssets[iface.NetworkId][address] = true
			}
		}
	}
	for ri := range *spec.Routers {
		router := &(*spec.Routers)[ri]
		if router.Id == "" {
			router.Id = uuid.NewString()
		}
		if objects[router.Id] {
			return Invalid("路由器 %s 的标识重复", router.Name)
		}
		objects[router.Id] = true
		router.Name = strings.TrimSpace(router.Name)
		if router.Name == "" {
			router.Name = "路由器"
		}
		if router.Interfaces == nil {
			router.Interfaces = []api.RouterInterface{}
		}
		connected := map[string]bool{}
		prefixes := []netip.Prefix{}
		for ii := range router.Interfaces {
			iface := &router.Interfaces[ii]
			if iface.Id == "" {
				iface.Id = uuid.NewString()
			}
			if interfaces[iface.Id] {
				return Invalid("路由器 %s 的接口标识重复", router.Name)
			}
			interfaces[iface.Id] = true
			network, exists := networks[iface.NetworkId]
			if !exists || connected[iface.NetworkId] {
				return Invalid("路由器 %s 的网段不存在或重复连接", router.Name)
			}
			prefix := netip.MustParsePrefix(network.Cidr)
			for _, previous := range prefixes {
				if prefix.Overlaps(previous) {
					return Invalid("路由器 %s 不能直接连接重叠网段 %s 和 %s", router.Name, previous, prefix)
				}
			}
			connected[iface.NetworkId] = true
			prefixes = append(prefixes, prefix)
			if iface.Address == "" {
				continue
			}
			address, err := netip.ParseAddr(iface.Address)
			if err != nil || !usable(pools[iface.NetworkId], address) || used[iface.NetworkId][address] {
				return Invalid("路由器 %s 的接口地址 %s 无效或已占用", router.Name, iface.Address)
			}
			iface.Address = address.String()
			used[iface.NetworkId][address] = true
		}
	}
	for ri := range *spec.Routers {
		for ii := range (*spec.Routers)[ri].Interfaces {
			iface := &(*spec.Routers)[ri].Interfaces[ii]
			if iface.Address != "" {
				continue
			}
			pool := pools[iface.NetworkId]
			address := pool.Addr().Next()
			if gateway := networks[iface.NetworkId].Gateway; gateway != nil {
				candidate, _ := netip.ParseAddr(*gateway)
				if usable(pool, candidate) && !used[iface.NetworkId][candidate] && !explicitAssets[iface.NetworkId][candidate] {
					address = candidate
				}
			}
			for usable(pool, address) && (used[iface.NetworkId][address] || explicitAssets[iface.NetworkId][address]) {
				address = address.Next()
			}
			if !usable(pool, address) {
				return Invalid("网段 %s 没有可用路由接口地址", networks[iface.NetworkId].Name)
			}
			iface.Address = address.String()
			used[iface.NetworkId][address] = true
		}
	}
	for i := range spec.Networks {
		network := &spec.Networks[i]
		if network.External != nil || network.Gateway != nil {
			continue
		}
		for _, router := range *spec.Routers {
			for _, iface := range router.Interfaces {
				if iface.NetworkId == network.Id && network.Gateway == nil {
					address := iface.Address
					network.Gateway = &address
				}
			}
		}
	}
	return normalizeRoutes(spec, networks)
}

func normalizeRoutes(spec *api.EnvironmentSpec, networks map[string]api.Network) error {
	if spec.Routes == nil {
		return nil
	}
	ports := map[string]map[string]api.RouterInterface{}
	for _, router := range *spec.Routers {
		ports[router.Id] = map[string]api.RouterInterface{}
		for _, iface := range router.Interfaces {
			ports[router.Id][iface.NetworkId] = iface
		}
	}
	seen := map[string]bool{}
	for i := range *spec.Routes {
		route := &(*spec.Routes)[i]
		if route.RouterId == nil || *route.RouterId == "" {
			route.RouterId = nil
			for _, router := range *spec.Routers {
				if _, exists := ports[router.Id][route.NetworkId]; exists {
					if route.RouterId != nil {
						return Invalid("路由出网段连接了多个路由器，请指定路由器")
					}
					id := router.Id
					route.RouterId = &id
				}
			}
		}
		if route.RouterId == nil {
			return Invalid("静态路由没有对应的路由器")
		}
		iface, exists := ports[*route.RouterId][route.NetworkId]
		if !exists {
			return Invalid("静态路由的出网段未连接所选路由器")
		}
		destination, err := netip.ParsePrefix(route.Destination)
		if err != nil {
			return Invalid("静态路由目标网段无效：%s", route.Destination)
		}
		next, err := netip.ParseAddr(route.NextHop)
		prefix := netip.MustParsePrefix(networks[route.NetworkId].Cidr)
		if err != nil || !usable(prefix, next) || next.Is4() != destination.Addr().Is4() || next.String() == iface.Address {
			return Invalid("静态路由下一跳不在出网段内或指向自身：%s", route.NextHop)
		}
		route.Destination, route.NextHop = destination.Masked().String(), next.String()
		key := *route.RouterId + "/" + route.Destination
		if seen[key] {
			return Invalid("路由表内目标网段重复：%s", route.Destination)
		}
		seen[key] = true
	}
	return nil
}

func reserveNetworkAccess(spec *api.EnvironmentSpec, pools map[string]netip.Prefix, used map[string]map[netip.Addr]bool) error {
	targets := map[string]bool{}
	for _, asset := range spec.Assets {
		for _, iface := range asset.Interfaces {
			targets[iface.NetworkId] = true
		}
	}
	for i := range spec.Networks {
		network := &spec.Networks[i]
		if network.Gateway != nil {
			address, _ := netip.ParseAddr(*network.Gateway)
			used[network.Id][address] = true
		}
		pool := pools[network.Id]
		if !targets[network.Id] && len(network.Devices) == 0 {
			network.AccessAddress = nil
			continue
		}
		address := netip.Addr{}
		if network.AccessAddress != nil {
			address, _ = netip.ParseAddr(*network.AccessAddress)
			if !usable(pool, address) || used[network.Id][address] {
				return Invalid("网段 %s 的平台管理地址无效或已占用", network.Name)
			}
		} else {
			bytes := pool.Masked().Addr().AsSlice()
			for bit := pool.Bits(); bit < len(bytes)*8; bit++ {
				bytes[bit/8] |= 1 << (7 - bit%8)
			}
			address, _ = netip.AddrFromSlice(bytes)
			if address.Is4() && pool.Bits() < 31 {
				address = address.Prev()
			}
			for usable(pool, address) && used[network.Id][address] {
				address = address.Prev()
			}
			if !usable(pool, address) {
				return Invalid("网段 %s 没有可用的平台管理地址", network.Name)
			}
		}
		value := address.String()
		network.AccessAddress = &value
		used[network.Id][address] = true
	}
	return nil
}
