package environment

import (
	"crypto/rand"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"strings"

	"github.com/google/uuid"
	"netlab.local/core/api"
	"netlab.local/core/internal/guest"
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
	attachments := map[string]bool{}
	for i := range spec.Networks {
		n := &spec.Networks[i]
		if n.Id == "" {
			n.Id = uuid.NewString()
		}
		if _, ok := networks[n.Id]; ok {
			return spec, Invalid("网段 %s 的标识重复", n.Name)
		}
		p, err := netip.ParsePrefix(n.Cidr)
		if err != nil {
			return spec, Invalid("网段 %s：%v", n.Name, err)
		}
		p = p.Masked()
		n.Cidr = p.String()
		gateway := netip.Addr{}
		if n.Gateway != nil && *n.Gateway != "" {
			gateway, err = netip.ParseAddr(*n.Gateway)
			if err != nil {
				return spec, Invalid("网段 %s 的网关地址无效", n.Name)
			}
		} else if n.External == nil {
			gateway = p.Addr().Next()
		}
		if gateway.IsValid() && !usable(p, gateway) && !(n.External != nil && p.Addr().Is6() && gateway.Is6() && gateway.IsLinkLocalUnicast()) {
			return spec, Invalid("网段 %s 没有可用网关地址", n.Name)
		}
		n.Gateway = nil
		if gateway.IsValid() {
			g := gateway.String()
			n.Gateway = &g
		}
		pool := p
		if n.External != nil {
			key := n.External.Key()
			if attachments[key] {
				return spec, Invalid("网段 %s 与其他网段重复使用外部接口和 VLAN", n.Name)
			}
			attachments[key] = true
			if n.External.NodeId == "" || n.External.Interface == "" || (n.External.Vlan != nil && (*n.External.Vlan < 1 || *n.External.Vlan > 4094)) {
				return spec, Invalid("网段 %s 的外部接口或 VLAN 无效", n.Name)
			}
			if n.AllocationPool == nil {
				return spec, Invalid("网段 %s 需要设置 Netlab 可分配地址段", n.Name)
			}
			pool, err = netip.ParsePrefix(*n.AllocationPool)
			if err != nil || pool.Bits() < p.Bits() || !p.Contains(pool.Addr()) {
				return spec, Invalid("网段 %s 的可分配地址段不在该 LAN 内", n.Name)
			}
			pool = pool.Masked()
			value := pool.String()
			n.AllocationPool = &value
			if !usable(pool, pool.Addr().Next().Next()) || gateway == pool.Addr().Next() {
				return spec, Invalid("网段 %s 的可分配地址段需要包含路由地址和资产地址", n.Name)
			}
		} else {
			n.AllocationPool = nil
		}
		if n.Mtu == nil {
			mtu := 1400
			n.Mtu = &mtu
		}
		if *n.Mtu < 1280 || *n.Mtu > 9000 {
			return spec, Invalid("网段 %s 的 MTU 应在 1280–9000 之间", n.Name)
		}
		networks[n.Id] = *n
		prefixes[n.Id] = pool
		addresses[n.Id] = map[netip.Addr]bool{gateway: true}
		if n.External != nil {
			addresses[n.Id][pool.Addr().Next()] = true
		}
	}
	assetIDs, interfaceIDs, macs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i := range spec.Assets {
		a := &spec.Assets[i]
		if a.Id == "" {
			a.Id = uuid.NewString()
		}
		if assetIDs[a.Id] {
			return spec, Invalid("资产 %s 的标识重复", a.Name)
		}
		assetIDs[a.Id] = true
		t, ok := templates[a.TemplateId]
		if !ok {
			return spec, Invalid("资产 %s 引用的模板不存在", a.Name)
		}
		if binding := a.PciBinding; binding != nil {
			if t.Kind != api.Vm || binding.NodeId == "" || len(binding.GroupIds) == 0 {
				return spec, Invalid("PCI 直通需要虚拟机、节点和设备组")
			}
			slices.Sort(binding.GroupIds)
			binding.GroupIds = slices.Compact(binding.GroupIds)
		}
		mediaIDs := []string{}
		if t.Media != nil {
			for _, media := range *t.Media {
				mediaIDs = append(mediaIDs, media.Id)
			}
		}
		if a.Media == nil && len(mediaIDs) > 0 {
			a.Media = &mediaIDs
		}
		if a.Media != nil {
			seen := map[string]bool{}
			for _, id := range *a.Media {
				if seen[id] || !slices.Contains(mediaIDs, id) {
					return spec, Invalid("资产 %s 的安装介质无效", a.Name)
				}
				seen[id] = true
			}
		}
		if a.RestartPolicy != nil {
			if t.Kind != api.Container || (*a.RestartPolicy != api.Never && *a.RestartPolicy != api.OnFailure && *a.RestartPolicy != api.Always) {
				return spec, Invalid("资产 %s 的容器重启策略无效", a.Name)
			}
		}
		if a.Volumes == nil && t.Volumes != nil {
			volumes := slices.Clone(*t.Volumes)
			a.Volumes = &volumes
		}
		if a.Volumes != nil {
			ids, paths := map[string]bool{}, map[string]bool{}
			for j := range *a.Volumes {
				volume := &(*a.Volumes)[j]
				if volume.Id == "" {
					volume.Id = uuid.NewString()
				}
				if ids[volume.Id] || paths[volume.MountPath] || volume.SizeGiB < 1 {
					return spec, Invalid("资产 %s 的数据卷配置无效", a.Name)
				}
				ids[volume.Id], paths[volume.MountPath] = true, true
			}
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
			return spec, Invalid("资产 %s 的资源规格无效", a.Name)
		}
		if err := guest.ValidateCPU(t.Hardware, a.Resources); err != nil {
			return spec, Invalid("资产 %s：%v", a.Name, err)
		}
		primary := 0
		for j := range a.Interfaces {
			nic := &a.Interfaces[j]
			if nic.Id == "" {
				nic.Id = uuid.NewString()
			}
			if interfaceIDs[nic.Id] {
				return spec, Invalid("资产 %s 的接口标识重复", a.Name)
			}
			interfaceIDs[nic.Id] = true
			p, ok := prefixes[nic.NetworkId]
			if !ok {
				return spec, Invalid("资产 %s 的接口引用了不存在的网段", a.Name)
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
				return spec, Invalid("资产 %s 的 MAC 地址无效", a.Name)
			}
			nic.Mac = mac.String()
			if macs[nic.NetworkId+"/"+nic.Mac] {
				return spec, Invalid("网段内 MAC 地址 %s 重复", nic.Mac)
			}
			macs[nic.NetworkId+"/"+nic.Mac] = true
			if nic.Address != "" {
				addr, err := netip.ParseAddr(nic.Address)
				if err != nil || !usable(p, addr) {
					return spec, Invalid("资产 %s 的地址 %s 不属于可用网段", a.Name, nic.Address)
				}
				if addresses[nic.NetworkId][addr] {
					return spec, Invalid("地址 %s 已占用", nic.Address)
				}
				addresses[nic.NetworkId][addr] = true
			}
		}
		if primary > 1 {
			return spec, Invalid("资产 %s 只能有一个默认出口", a.Name)
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
				return spec, Invalid("网段 %s 没有可用地址", networks[nic.NetworkId].Name)
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
				return spec, Invalid("网段 %s 的 DNS 资产未接入该网段", n.Name)
			}
		}
	}
	return normalizeServices(spec)
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

func RequiresStop(kind api.TemplateKind, before, after api.Asset) bool {
	before.Resources = after.Resources
	before.RestartPolicy = after.RestartPolicy
	return string(kind) == "vm" || !reflect.DeepEqual(before, after)
}

func RequiresReplacement(template api.Template, before, after api.Asset, networkChanged bool) bool {
	return before.TemplateId != after.TemplateId || !reflect.DeepEqual(before.Guest, after.Guest) || !reflect.DeepEqual(before.PciBinding, after.PciBinding) ||
		(template.Initialization != nil && *template.Initialization == "cloudbase-init" && networkChanged)
}

func Diff(revision int, before, after api.EnvironmentSpec, templates map[string]api.Template) api.ChangePreview {
	result := api.ChangePreview{Revision: revision, Changes: []api.ChangeItem{}}
	oldAssets := map[string]api.Asset{}
	for _, a := range before.Assets {
		oldAssets[a.Id] = a
	}
	for _, a := range after.Assets {
		old, ok := oldAssets[a.Id]
		effect := api.Add
		networkChanged := false
		if ok {
			delete(oldAssets, a.Id)
			previous := Resolve(before, old, nil)
			networkChanged = !reflect.DeepEqual(previous, Resolve(after, a, previous))
			if reflect.DeepEqual(old, a) && !networkChanged {
				continue
			}
			effect = api.Update
			if RequiresReplacement(templates[a.TemplateId], old, a, networkChanged) {
				effect = api.Replace
			}
		}
		needsStop := ok && (effect == api.Replace || networkChanged || RequiresStop(templates[a.TemplateId].Kind, old, a))
		item := api.ChangeItem{Id: a.Id, Name: a.Name, Kind: api.ChangeItemKindAsset, Effect: effect, RequiresStop: &needsStop}
		if effect == api.Replace {
			impact := "重建系统盘，保留数据卷"
			item.DataEffect = &impact
		}
		result.Changes = append(result.Changes, item)
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
	oldServices := map[string]api.ServiceExposure{}
	for _, service := range Services(before) {
		oldServices[service.Id] = service
	}
	for _, service := range Services(after) {
		previous, exists := oldServices[service.Id]
		delete(oldServices, service.Id)
		effect := api.Add
		if exists {
			if reflect.DeepEqual(previous, service) {
				continue
			}
			effect = api.Update
		}
		result.Changes = append(result.Changes, api.ChangeItem{Id: service.Id, Name: "服务开放", Kind: api.ChangeItemKindService, Effect: effect})
	}
	for _, service := range oldServices {
		result.Changes = append(result.Changes, api.ChangeItem{Id: service.Id, Name: "服务开放", Kind: api.ChangeItemKindService, Effect: api.Remove})
	}
	slices.SortFunc(result.Changes, func(a, b api.ChangeItem) int { return strings.Compare(a.Id, b.Id) })
	return result
}
