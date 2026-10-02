package environment

import (
	"crypto/rand"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"strings"

	"github.com/google/uuid"
	"netlab.local/core/api"
)

// Normalize assigns identities and addresses once; saved specifications carry them forward.
func Normalize(spec api.EnvironmentSpec, templates map[string]api.Template) (api.EnvironmentSpec, error) {
	if spec.Assets == nil {
		spec.Assets = []api.Asset{}
	}
	if spec.Networks == nil {
		spec.Networks = []api.Network{}
	}
	networks := map[string]api.Network{}
	prefixes := map[string]netip.Prefix{}
	addresses := map[string]map[netip.Addr]bool{}
	for i := range spec.Networks {
		n := &spec.Networks[i]
		if n.Id == "" {
			n.Id = uuid.NewString()
		}
		if _, ok := networks[n.Id]; ok {
			return spec, fmt.Errorf("网段 %s 的标识重复", n.Name)
		}
		p, err := netip.ParsePrefix(n.Cidr)
		if err != nil {
			return spec, fmt.Errorf("网段 %s：%w", n.Name, err)
		}
		p = p.Masked()
		n.Cidr = p.String()
		gateway := p.Addr().Next()
		if n.Gateway != nil {
			gateway, err = netip.ParseAddr(*n.Gateway)
			if err != nil {
				return spec, err
			}
		}
		if !usable(p, gateway) {
			return spec, fmt.Errorf("网段 %s 没有可用网关地址", n.Name)
		}
		g := gateway.String()
		n.Gateway = &g
		if n.Mtu == nil {
			mtu := 1400
			n.Mtu = &mtu
		}
		if *n.Mtu < 1280 || *n.Mtu > 9000 {
			return spec, fmt.Errorf("网段 %s 的 MTU 应在 1280–9000 之间", n.Name)
		}
		networks[n.Id] = *n
		prefixes[n.Id] = p
		addresses[n.Id] = map[netip.Addr]bool{gateway: true}
	}
	assetIDs, interfaceIDs, macs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i := range spec.Assets {
		a := &spec.Assets[i]
		if a.Id == "" {
			a.Id = uuid.NewString()
		}
		if assetIDs[a.Id] {
			return spec, fmt.Errorf("资产 %s 的标识重复", a.Name)
		}
		assetIDs[a.Id] = true
		t, ok := templates[a.TemplateId]
		if !ok {
			return spec, fmt.Errorf("资产 %s 引用的模板不存在", a.Name)
		}
		if strings.TrimSpace(a.Name) == "" {
			a.Name = t.Name
		}
		if a.Resources.Cpu == 0 {
			a.Resources.Cpu = t.Resources.Cpu
		}
		if a.Resources.MemoryMiB == 0 {
			a.Resources.MemoryMiB = t.Resources.MemoryMiB
		}
		if a.Resources.DiskGiB == 0 {
			a.Resources.DiskGiB = t.Resources.DiskGiB
		}
		if a.Resources.Cpu < 1 || a.Resources.MemoryMiB < 64 || a.Resources.DiskGiB < 1 {
			return spec, fmt.Errorf("资产 %s 的资源规格无效", a.Name)
		}
		primary := 0
		for j := range a.Interfaces {
			nic := &a.Interfaces[j]
			if nic.Id == "" {
				nic.Id = uuid.NewString()
			}
			if interfaceIDs[nic.Id] {
				return spec, fmt.Errorf("资产 %s 的接口标识重复", a.Name)
			}
			interfaceIDs[nic.Id] = true
			p, ok := prefixes[nic.NetworkId]
			if !ok {
				return spec, fmt.Errorf("资产 %s 的接口引用了不存在的网段", a.Name)
			}
			if nic.Primary {
				primary++
			}
			if nic.Mac == "" {
				raw := make([]byte, 6)
				if _, err := rand.Read(raw); err != nil {
					return spec, err
				}
				raw[0] = (raw[0] | 2) & 0xfe
				nic.Mac = net.HardwareAddr(raw).String()
			}
			mac, err := net.ParseMAC(nic.Mac)
			if err != nil || len(mac) != 6 {
				return spec, fmt.Errorf("资产 %s 的 MAC 地址无效", a.Name)
			}
			nic.Mac = mac.String()
			if macs[nic.NetworkId+"/"+nic.Mac] {
				return spec, fmt.Errorf("网段内 MAC 地址 %s 重复", nic.Mac)
			}
			macs[nic.NetworkId+"/"+nic.Mac] = true
			if nic.Address != "" {
				addr, err := netip.ParseAddr(nic.Address)
				if err != nil || !usable(p, addr) {
					return spec, fmt.Errorf("资产 %s 的地址 %s 不属于可用网段", a.Name, nic.Address)
				}
				if addresses[nic.NetworkId][addr] {
					return spec, fmt.Errorf("地址 %s 已占用", nic.Address)
				}
				addresses[nic.NetworkId][addr] = true
			}
		}
		if primary > 1 {
			return spec, fmt.Errorf("资产 %s 只能有一个默认出口", a.Name)
		}
		if primary == 0 && len(a.Interfaces) > 0 {
			a.Interfaces[0].Primary = true
		}
	}
	for i := range spec.Assets {
		for j := range spec.Assets[i].Interfaces {
			nic := &spec.Assets[i].Interfaces[j]
			if nic.Address != "" {
				continue
			}
			p := prefixes[nic.NetworkId]
			addr := p.Addr().Next()
			for usable(p, addr) && addresses[nic.NetworkId][addr] {
				addr = addr.Next()
			}
			if !usable(p, addr) {
				return spec, fmt.Errorf("网段 %s 没有可用地址", networks[nic.NetworkId].Name)
			}
			nic.Address = addr.String()
			addresses[nic.NetworkId][addr] = true
		}
	}
	for _, n := range spec.Networks {
		if n.DnsAssetId != nil && *n.DnsAssetId != "" {
			found := false
			for _, a := range spec.Assets {
				if a.Id == *n.DnsAssetId {
					for _, nic := range a.Interfaces {
						if nic.NetworkId == n.Id {
							found = true
						}
					}
				}
			}
			if !found {
				return spec, fmt.Errorf("网段 %s 的 DNS 资产未接入该网段", n.Name)
			}
		}
	}
	return spec, nil
}

func usable(p netip.Prefix, a netip.Addr) bool {
	if !a.IsValid() || !p.Contains(a) || a == p.Addr() {
		return false
	}
	return !a.Is4() || p.Bits() >= 31 || p.Contains(a.Next())
}

func Resolve(spec api.EnvironmentSpec, asset api.Asset, previous []api.ResolvedInterface) []api.ResolvedInterface {
	networks := map[string]api.Network{}
	for _, n := range spec.Networks {
		networks[n.Id] = n
	}
	previousPorts := map[string]string{}
	for _, nic := range previous {
		previousPorts[nic.Id] = nic.PortName
	}
	result := make([]api.ResolvedInterface, 0, len(asset.Interfaces))
	for _, nic := range asset.Interfaces {
		n := networks[nic.NetworkId]
		p, _ := netip.ParsePrefix(n.Cidr)
		port := previousPorts[nic.Id]
		if port == "" {
			port = uuid.NewString()
		}
		dns := []string{}
		if n.DnsServers != nil {
			dns = append(dns, (*n.DnsServers)...)
		}
		if n.DnsAssetId != nil {
			for _, a := range spec.Assets {
				if a.Id == *n.DnsAssetId {
					for _, d := range a.Interfaces {
						if d.NetworkId == n.Id {
							dns = []string{d.Address}
						}
					}
				}
			}
		}
		resolved := api.ResolvedInterface{Id: nic.Id, NetworkId: nic.NetworkId, Mac: nic.Mac, Address: nic.Address, Prefix: p.Bits(), Primary: nic.Primary, PortName: port, Mtu: *n.Mtu, Dns: &dns}
		if nic.Primary {
			resolved.Gateway = n.Gateway
		}
		result = append(result, resolved)
	}
	return result
}

func Diff(revision int, before, after api.EnvironmentSpec) api.ChangePreview {
	result := api.ChangePreview{Revision: revision, Changes: []api.ChangeItem{}}
	oldAssets := map[string]api.Asset{}
	for _, a := range before.Assets {
		oldAssets[a.Id] = a
	}
	for _, a := range after.Assets {
		old, ok := oldAssets[a.Id]
		effect := api.Add
		if ok {
			delete(oldAssets, a.Id)
			if reflect.DeepEqual(old, a) {
				continue
			}
			effect = api.Update
			if old.TemplateId != a.TemplateId {
				effect = api.Replace
			}
		}
		needsStop := ok
		result.Changes = append(result.Changes, api.ChangeItem{Id: a.Id, Name: a.Name, Kind: api.ChangeItemKindAsset, Effect: effect, RequiresStop: &needsStop})
	}
	for _, a := range oldAssets {
		stop := true
		result.Changes = append(result.Changes, api.ChangeItem{Id: a.Id, Name: a.Name, Kind: api.ChangeItemKindAsset, Effect: api.Remove, RequiresStop: &stop})
	}
	oldNetworks := map[string]api.Network{}
	for _, n := range before.Networks {
		oldNetworks[n.Id] = n
	}
	for _, n := range after.Networks {
		old, ok := oldNetworks[n.Id]
		effect := api.Add
		if ok {
			delete(oldNetworks, n.Id)
			if reflect.DeepEqual(old, n) {
				continue
			}
			effect = api.Update
		}
		result.Changes = append(result.Changes, api.ChangeItem{Id: n.Id, Name: n.Name, Kind: api.ChangeItemKindNetwork, Effect: effect})
	}
	for _, n := range oldNetworks {
		result.Changes = append(result.Changes, api.ChangeItem{Id: n.Id, Name: n.Name, Kind: api.ChangeItemKindNetwork, Effect: api.Remove})
	}
	if !reflect.DeepEqual(before.Routes, after.Routes) || !reflect.DeepEqual(before.Policies, after.Policies) {
		result.Changes = append(result.Changes, api.ChangeItem{Id: "network-rules", Name: "网络规则", Kind: api.ChangeItemKindNetwork, Effect: api.Update})
	}
	slices.SortFunc(result.Changes, func(a, b api.ChangeItem) int { return strings.Compare(a.Id, b.Id) })
	return result
}
