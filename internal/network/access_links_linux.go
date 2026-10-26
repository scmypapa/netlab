//go:build linux

package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
	"netlab.local/core/api"
)

func (v *Access) serviceRecord(environment string) gatewayRecord {
	if v.ovn.gatewayRecord == nil {
		return gatewayRecord{}
	}
	return v.ovn.gatewayRecord(environment)
}

func (v *Access) applyServiceBindings(ctx context.Context, plan api.NodePlan, before gatewayRecord) error {
	v.mu.RLock()
	previous, exists := v.records[plan.EnvironmentId]
	v.mu.RUnlock()
	if !exists {
		return fmt.Errorf("environment access has not been prepared")
	}
	next := previous
	if len(plan.Spec.Networks) > 0 {
		next.Networks = slices.Clone(plan.Spec.Networks)
	}
	services := gatewayRecord{Services: *plan.Services}
	if plan.Gateway != nil {
		services.Gateway = *plan.Gateway
	}
	if err := v.apply(ctx, plan.EnvironmentId, &next, previous, services); err != nil {
		return err
	}
	if ports := changedPorts(before, services); len(ports) > 0 {
		handle, err := ns.GetNS("/run/netns/" + accessName(plan.EnvironmentId))
		if err != nil {
			return err
		}
		defer handle.Close()
		if err = handle.Do(func(_ ns.NetNS) error {
			return clearBindingConnections(netip.MustParseAddr(before.Gateway.Address), ports)
		}); err != nil {
			return err
		}
	}
	if err := v.writeRecord(plan.EnvironmentId, next); err != nil {
		return err
	}
	v.mu.Lock()
	v.records[plan.EnvironmentId] = next
	v.mu.Unlock()
	return nil
}

func (v *Access) removeAccessPorts(ctx context.Context, environment string, wanted map[string]bool) error {
	var ports []Port
	if err := v.ovs.client.WhereCache(func(port *Port) bool {
		return port.ExternalIDs["netlab.environment"] == environment && port.ExternalIDs["netlab.asset"] == "access"
	}).List(ctx, &ports); err != nil {
		return err
	}
	for _, port := range ports {
		if wanted[port.Name] {
			continue
		}
		if _, err := v.ovs.Detach(ctx, port.Name, environment, "access", port.ExternalIDs["netlab.instance"]); err != nil {
			return err
		}
		link, err := netlink.LinkByName(port.Name)
		var missing netlink.LinkNotFoundError
		if errors.As(err, &missing) {
			continue
		}
		if err != nil {
			return err
		}
		if err = netlink.LinkDel(link); err != nil {
			return err
		}
	}
	return nil
}

func (v *Access) connect(ctx context.Context, environment string, handle ns.NetNS, record *accessRecord, services gatewayRecord) error {
	wanted := map[string]bool{}
	for _, network := range record.Networks {
		if network.AccessAddress == nil {
			continue
		}
		id := accessLinkID(environment, network.Id)
		host, peer, vrfName := "nh"+id, "nm"+id, "vr"+id
		wanted[host] = true
		if err := v.accessVeth(ctx, handle, environment, host, peer, accessNetworkPort(environment, network.Id), network.Id); err != nil {
			return err
		}
		if err := handle.Do(func(_ ns.NetNS) error {
			var missing netlink.LinkNotFoundError
			vrf, err := netlink.LinkByName(vrfName)
			if errors.As(err, &missing) {
				links, listErr := netlink.LinkList()
				if listErr != nil {
					return listErr
				}
				used := map[uint32]bool{}
				for _, link := range links {
					if existing, ok := link.(*netlink.Vrf); ok {
						used[existing.Table] = true
					}
				}
				table := uint32(1000)
				for used[table] {
					table++
				}
				if table > 65535 {
					return fmt.Errorf("environment network routing tables exhausted")
				}
				vrf = &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: vrfName}, Table: table}
				if err = netlink.LinkAdd(vrf); err != nil {
					return err
				}
				vrf, err = netlink.LinkByName(vrfName)
			}
			if err != nil {
				return err
			}
			if err = netlink.LinkSetUp(vrf); err != nil {
				return err
			}
			link, err := netlink.LinkByName(peer)
			if err != nil {
				return err
			}
			if err = netlink.LinkSetMaster(link, vrf); err != nil {
				return err
			}
			prefix := netip.MustParsePrefix(network.Cidr)
			address := netip.MustParseAddr(*network.AccessAddress)
			mac, _ := net.ParseMAC(routerMAC(address))
			if err = netlink.LinkSetHardwareAddr(link, mac); err != nil {
				return err
			}
			if err = replaceAccessAddresses(link, []string{netip.PrefixFrom(address, prefix.Bits()).String()}, 0); err != nil {
				return err
			}
			if err = netlink.LinkSetUp(link); err != nil {
				return err
			}
			table := int(vrf.(*netlink.Vrf).Table)
			for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
				rules, err := netlink.RuleList(family)
				if err != nil {
					return err
				}
				found := slices.ContainsFunc(rules, func(rule netlink.Rule) bool {
					return rule.Priority == 100 && rule.Table == table && rule.Mark == uint32(table)
				})
				if !found {
					rule := netlink.NewRule()
					rule.Family = family
					rule.Priority = 100
					rule.Table = table
					rule.Mark = uint32(table)
					if err = netlink.RuleAdd(rule); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			return fmt.Errorf("network %s VRF: %w", network.Name, err)
		}
	}
	if services.Gateway.Address != "" {
		wanted[accessDevice(environment)] = true
		if err := v.accessVeth(ctx, handle, environment, accessDevice(environment), "access0", accessPort(environment), "access"); err != nil {
			return err
		}
		if err := handle.Do(func(_ ns.NetNS) error {
			link, err := netlink.LinkByName("access0")
			if err != nil {
				return err
			}
			address := netip.MustParseAddr(services.Gateway.Address)
			mac, _ := net.ParseMAC(routerMAC(address))
			if err = netlink.LinkSetHardwareAddr(link, mac); err != nil {
				return err
			}
			if err = replaceAccessAddresses(link, []string{netip.PrefixFrom(address, v.ovn.provider.Bits()).String()}, 0); err != nil {
				return err
			}
			return netlink.LinkSetUp(link)
		}); err != nil {
			return err
		}
	}
	if err := v.removeAccessPorts(ctx, environment, wanted); err != nil {
		return err
	}
	return handle.Do(func(_ ns.NetNS) error {
		links, err := netlink.LinkList()
		if err != nil {
			return err
		}
		tables := map[int]bool{}
		for _, link := range links {
			vrf, ok := link.(*netlink.Vrf)
			if !ok {
				continue
			}
			keep := slices.ContainsFunc(record.Networks, func(network api.Network) bool {
				return network.AccessAddress != nil && vrf.Name == "vr"+accessLinkID(environment, network.Id)
			})
			if !keep {
				if err = netlink.LinkDel(vrf); err != nil {
					return err
				}
			} else {
				tables[int(vrf.Table)] = true
			}
		}
		for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
			rules, err := netlink.RuleList(family)
			if err != nil {
				return err
			}
			for _, rule := range rules {
				if rule.Priority == 100 && !tables[rule.Table] {
					if err = netlink.RuleDel(&rule); err != nil {
						return err
					}
				}
			}
		}
		for _, path := range []string{"/proc/sys/net/ipv4/ip_forward", "/proc/sys/net/ipv6/conf/all/forwarding"} {
			if err = os.WriteFile(path, []byte("1"), 0644); err != nil {
				return err
			}
		}
		for _, path := range []string{"/proc/sys/net/ipv4/conf/all/rp_filter", "/proc/sys/net/ipv4/conf/default/rp_filter"} {
			if err = os.WriteFile(path, []byte("0"), 0644); err != nil {
				return err
			}
		}
		return nil
	})
}

func (v *Access) accessVeth(ctx context.Context, handle ns.NetNS, environment, hostName, peerName, portName, networkID string) error {
	host, err := netlink.LinkByName(hostName)
	var missing netlink.LinkNotFoundError
	if errors.As(err, &missing) {
		if err = netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: hostName, MTU: 1400}, PeerName: peerName, PeerNamespace: netlink.NsFd(handle.Fd())}); err != nil {
			return err
		}
		host, err = netlink.LinkByName(hostName)
	}
	if err != nil {
		return err
	}
	if err = netlink.LinkSetUp(host); err != nil {
		return err
	}
	return v.ovs.Attach(ctx, hostName, portName, environment, "access", networkID)
}

type accessLink struct {
	network api.Network
	device  string
	vrf     string
	table   uint32
}

func accessLinks(environment string, record *accessRecord) (map[string]accessLink, error) {
	result := map[string]accessLink{}
	for _, network := range record.Networks {
		if network.AccessAddress == nil {
			continue
		}
		id := accessLinkID(environment, network.Id)
		link, err := netlink.LinkByName("vr" + id)
		if err != nil {
			return nil, err
		}
		vrf := link.(*netlink.Vrf)
		result[network.Id] = accessLink{network, "nm" + id, vrf.Name, vrf.Table}
	}
	return result, nil
}
