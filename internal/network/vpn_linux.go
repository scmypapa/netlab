//go:build linux

package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"netlab.local/core/api"
)

type VPN struct {
	mu        sync.RWMutex
	directory string
	ovs       *OVS
	ovn       *OVN
	records   map[string]vpnRecord
}

func NewVPN(ctx context.Context, directory string, ovs *OVS, ovn *OVN) (*VPN, error) {
	v := &VPN{directory: filepath.Join(directory, "vpn"), ovs: ovs, ovn: ovn, records: map[string]vpnRecord{}}
	if err := os.MkdirAll(v.directory, 0700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(v.directory)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(v.directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		var record vpnRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, err
		}
		v.records[entry.Name()[:len(entry.Name())-5]] = record
	}
	ovn.vpnRecord = func(environment string) *vpnRecord {
		v.mu.RLock()
		defer v.mu.RUnlock()
		record, exists := v.records[environment]
		if !exists {
			return nil
		}
		return &record
	}
	for environment, record := range v.records {
		if len(record.Peers) == 0 {
			continue
		}
		if err := v.apply(ctx, environment, &record, record); err != nil {
			return nil, fmt.Errorf("restore VPN environment %s: %w", environment, err)
		}
		if err := v.writeRecord(environment, record); err != nil {
			return nil, err
		}
		v.records[environment] = record
	}
	return v, nil
}

func (v *VPN) writeRecord(environment string, record vpnRecord) error {
	file, err := os.CreateTemp(v.directory, ".vpn-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	err = json.NewEncoder(file).Encode(record)
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err == nil {
		err = os.Rename(file.Name(), filepath.Join(v.directory, environment+".json"))
	}
	return err
}

func (v *VPN) Apply(ctx context.Context, plan api.NodePlan) (api.NodeVPNResult, error) {
	if plan.Vpn == nil {
		return api.NodeVPNResult{}, fmt.Errorf("VPN application requires the complete peer set")
	}
	v.mu.RLock()
	previous, exists := v.records[plan.EnvironmentId]
	v.mu.RUnlock()
	if !exists {
		if len(plan.Vpn.Peers) == 0 {
			return api.NodeVPNResult{Mtu: vpnMTU, Peers: []api.NodeVPNPeer{}}, nil
		}
		key, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			return api.NodeVPNResult{}, err
		}
		transit, clients, err := vpnRanges(plan)
		if err != nil {
			return api.NodeVPNResult{}, err
		}
		previous = vpnRecord{PrivateKey: key.String(), Transit: transit, Clients: clients, Networks: slices.Clone(plan.Spec.Networks)}
		// The server key belongs to the environment, including while it has no peers.
		if err := v.writeRecord(plan.EnvironmentId, previous); err != nil {
			return api.NodeVPNResult{}, err
		}
		v.mu.Lock()
		v.records[plan.EnvironmentId] = previous
		v.mu.Unlock()
	}
	next := previous
	next.Networks = slices.Clone(plan.Spec.Networks)
	var err error
	if len(plan.Vpn.Peers) > 0 {
		if err = vpnRangeConflict(previous, plan); err != nil {
			if len(previous.Peers) > 0 {
				return api.NodeVPNResult{}, err
			}
			if next.Transit, next.Clients, err = vpnRanges(plan); err != nil {
				return api.NodeVPNResult{}, err
			}
		}
	}
	if next.Peers, err = assignVPNPeers(next, plan.Vpn.Peers); err != nil {
		return api.NodeVPNResult{}, err
	}
	next.ListenPort = plan.Vpn.ListenPort
	if next.ListenPort == 0 && len(next.Peers) > 0 {
		next.ListenPort = previous.ListenPort
	}
	rollback := func(cause error) (api.NodeVPNResult, error) {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		return api.NodeVPNResult{}, errors.Join(cause, v.apply(cleanup, plan.EnvironmentId, &previous, next))
	}
	if err := v.apply(ctx, plan.EnvironmentId, &next, previous); err != nil {
		return rollback(err)
	}
	if err := v.writeRecord(plan.EnvironmentId, next); err != nil {
		return rollback(err)
	}
	v.mu.Lock()
	v.records[plan.EnvironmentId] = next
	v.mu.Unlock()
	key, _ := wgtypes.ParseKey(next.PrivateKey)
	result := api.NodeVPNResult{PublicKey: key.PublicKey().String(), ListenPort: next.ListenPort, Mtu: vpnMTU, Peers: make([]api.NodeVPNPeer, 0, len(next.Peers))}
	for _, peer := range next.Peers {
		result.Peers = append(result.Peers, api.NodeVPNPeer{Id: peer.Peer.Id, Addresses: peer.Addresses})
	}
	return result, nil
}

func (v *VPN) apply(ctx context.Context, environment string, record *vpnRecord, previous vpnRecord) error {
	if len(record.Peers) == 0 {
		record.ListenPort = 0
		if err := v.ovn.applyVPN(ctx, environment, nil); err != nil {
			return err
		}
		return v.removeKernel(ctx, environment)
	}
	handle, err := vpnNamespace(environment)
	if err != nil {
		return err
	}
	defer handle.Close()
	if err = v.connect(ctx, environment, handle, record); err != nil {
		return err
	}
	if err = handle.Do(func(_ ns.NetNS) error { return applyVPNKernel(record) }); err != nil {
		return err
	}
	if err = v.ovn.applyVPN(ctx, environment, record); err != nil {
		return err
	}
	return handle.Do(func(_ ns.NetNS) error {
		client, err := wgctrl.New()
		if err != nil {
			return err
		}
		defer client.Close()
		device, err := client.Device("wg0")
		if err != nil {
			return err
		}
		key, err := wgtypes.ParseKey(record.PrivateKey)
		if err != nil {
			return err
		}
		config := wgtypes.Config{PrivateKey: &key, ListenPort: &record.ListenPort}
		desired := map[wgtypes.Key]bool{}
		for _, peer := range record.Peers {
			public, err := wgtypes.ParseKey(peer.Peer.PublicKey)
			if err != nil {
				return err
			}
			item := wgtypes.PeerConfig{PublicKey: public, ReplaceAllowedIPs: true}
			desired[public] = true
			for _, value := range peer.Addresses {
				_, prefix, _ := net.ParseCIDR(value)
				item.AllowedIPs = append(item.AllowedIPs, *prefix)
			}
			config.Peers = append(config.Peers, item)
		}
		// Apply the complete desired set without discarding unaffected peers' handshakes.
		for _, peer := range device.Peers {
			if !desired[peer.PublicKey] {
				config.Peers = append(config.Peers, wgtypes.PeerConfig{PublicKey: peer.PublicKey, Remove: true})
			}
		}
		if err = client.ConfigureDevice("wg0", config); err != nil {
			return err
		}
		device, err = client.Device("wg0")
		if err != nil {
			return err
		}
		record.ListenPort = device.ListenPort
		return clearChangedVPNConnections(previous, *record)
	})
}

func vpnNamespace(environment string) (ns.NetNS, error) {
	path := filepath.Join("/run/netns", vpnName(environment))
	if _, err := os.Stat(path); err == nil {
		return ns.GetNS(path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	current, err := netns.Get()
	if err != nil {
		return nil, err
	}
	defer current.Close()
	created, err := netns.NewNamed(vpnName(environment))
	restore := netns.Set(current)
	if created.IsOpen() {
		created.Close()
	}
	if err = errors.Join(err, restore); err != nil {
		return nil, err
	}
	return ns.GetNS(path)
}

func (v *VPN) connect(ctx context.Context, environment string, handle ns.NetNS, record *vpnRecord) error {
	var missing netlink.LinkNotFoundError
	hostName := vpnDevice(environment)
	host, err := netlink.LinkByName(hostName)
	if errors.As(err, &missing) {
		link := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: hostName, MTU: 1400}, PeerName: "vpn0", PeerNamespace: netlink.NsFd(handle.Fd())}
		if err = netlink.LinkAdd(link); err != nil {
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
	if err = v.ovs.Attach(ctx, hostName, vpnPort(environment), environment, "vpn", "vpn"); err != nil {
		return err
	}
	hasWG := false
	if err = handle.Do(func(_ ns.NetNS) error {
		_, err := netlink.LinkByName("wg0")
		hasWG = err == nil
		if errors.As(err, &missing) {
			return nil
		}
		return err
	}); err != nil {
		return err
	}
	if !hasWG {
		// Creation in the host namespace keeps encrypted UDP on the host's routing table.
		wireguard := &netlink.GenericLink{LinkAttrs: netlink.LinkAttrs{Name: "nw" + hostName[2:]}, LinkType: "wireguard"}
		if err = netlink.LinkAdd(wireguard); err != nil {
			return err
		}
		if err = netlink.LinkSetNsFd(wireguard, int(handle.Fd())); err != nil {
			return errors.Join(err, netlink.LinkDel(wireguard))
		}
	}
	return handle.Do(func(_ ns.NetNS) error {
		peer, err := netlink.LinkByName("vpn0")
		if err != nil {
			return err
		}
		address := netip.MustParsePrefix(record.Transit[0]).Addr().Next().Next()
		mac, _ := net.ParseMAC(routerMAC(address))
		if err = netlink.LinkSetHardwareAddr(peer, mac); err != nil {
			return err
		}
		for _, value := range record.Transit {
			prefix := netip.MustParsePrefix(value)
			address, _ := netlink.ParseAddr(netip.PrefixFrom(prefix.Addr().Next().Next(), prefix.Bits()).String())
			address.Flags = unix.IFA_F_NODAD
			if err = netlink.AddrReplace(peer, address); err != nil {
				return err
			}
		}
		if err = netlink.LinkSetUp(peer); err != nil {
			return err
		}
		wg, err := netlink.LinkByName("wg0")
		if !hasWG {
			wg, err = netlink.LinkByName("nw" + hostName[2:])
			if err == nil {
				err = netlink.LinkSetName(wg, "wg0")
			}
		}
		if err != nil {
			return err
		}
		if err = netlink.LinkSetMTU(wg, vpnMTU); err != nil {
			return err
		}
		for _, value := range record.Clients {
			prefix := netip.MustParsePrefix(value)
			address, _ := netlink.ParseAddr(netip.PrefixFrom(prefix.Addr().Next(), prefix.Bits()).String())
			address.Flags = unix.IFA_F_NODAD
			if err = netlink.AddrReplace(wg, address); err != nil {
				return err
			}
		}
		if err = netlink.LinkSetUp(wg); err != nil {
			return err
		}
		desired := make(map[string]bool, len(record.Networks))
		for _, network := range record.Networks {
			desired[netip.MustParsePrefix(network.Cidr).Masked().String()] = true
		}
		routes, err := netlink.RouteList(peer, netlink.FAMILY_ALL)
		if err != nil {
			return err
		}
		for _, route := range routes {
			if route.Gw == nil || (route.Dst != nil && desired[route.Dst.String()]) {
				continue
			}
			if err = netlink.RouteDel(&route); err != nil {
				return err
			}
		}
		for _, network := range record.Networks {
			prefix := netip.MustParsePrefix(network.Cidr)
			for _, value := range record.Transit {
				transit := netip.MustParsePrefix(value)
				if transit.Addr().Is4() != prefix.Addr().Is4() {
					continue
				}
				_, dst, _ := net.ParseCIDR(prefix.String())
				if err = netlink.RouteReplace(&netlink.Route{LinkIndex: peer.Attrs().Index, Dst: dst, Gw: net.IP(transit.Addr().Next().AsSlice())}); err != nil {
					return err
				}
			}
		}
		for _, path := range []string{"/proc/sys/net/ipv4/ip_forward", "/proc/sys/net/ipv6/conf/all/forwarding"} {
			if err = os.WriteFile(path, []byte("1"), 0644); err != nil {
				return err
			}
		}
		return nil
	})
}

func (v *VPN) removeKernel(ctx context.Context, environment string) error {
	if _, err := os.Stat(filepath.Join("/run/netns", vpnName(environment))); err == nil {
		handle, err := ns.GetNS(filepath.Join("/run/netns", vpnName(environment)))
		if err != nil {
			return err
		}
		err = handle.Do(func(_ ns.NetNS) error {
			return errors.Join(clearVPNConnections(netlink.FAMILY_V4), clearVPNConnections(netlink.FAMILY_V6))
		})
		handle.Close()
		if err != nil {
			return err
		}
		if err := netns.DeleteNamed(vpnName(environment)); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := v.ovs.Detach(ctx, vpnDevice(environment), environment, "vpn", "vpn"); err != nil {
		return err
	}
	link, err := netlink.LinkByName(vpnDevice(environment))
	var missing netlink.LinkNotFoundError
	if errors.As(err, &missing) {
		return nil
	}
	if err != nil {
		return err
	}
	return netlink.LinkDel(link)
}

func (v *VPN) Remove(ctx context.Context, environment string) error {
	if err := v.removeKernel(ctx, environment); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(v.directory, environment+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	v.mu.Lock()
	delete(v.records, environment)
	v.mu.Unlock()
	return nil
}

type allVPNConnections struct{}

func (allVPNConnections) MatchConntrackFlow(*netlink.ConntrackFlow) bool { return true }
func clearVPNConnections(family netlink.InetFamily) error {
	_, err := netlink.ConntrackDeleteFilters(netlink.ConntrackTable, family, allVPNConnections{})
	return err
}

type vpnConnections map[netip.Addr]bool

func (filter vpnConnections) MatchConntrackFlow(flow *netlink.ConntrackFlow) bool {
	address, _ := netip.AddrFromSlice(flow.Forward.SrcIP)
	return filter[address.Unmap()]
}

func clearChangedVPNConnections(previous, next vpnRecord) error {
	current := map[string]vpnPeer{}
	for _, peer := range next.Peers {
		current[peer.Peer.Id] = peer
	}
	changed := vpnConnections{}
	for _, peer := range previous.Peers {
		value, exists := current[peer.Peer.Id]
		if exists && peer.Peer.PublicKey == value.Peer.PublicKey && peer.Peer.Mode == value.Peer.Mode && slices.Equal(peer.Addresses, value.Addresses) && reflect.DeepEqual(peer.Peer.Routes, value.Peer.Routes) {
			continue
		}
		for _, value := range peer.Addresses {
			changed[netip.MustParsePrefix(value).Addr()] = true
		}
	}
	if len(changed) == 0 {
		return nil
	}
	_, v4 := netlink.ConntrackDeleteFilters(netlink.ConntrackTable, netlink.FAMILY_V4, changed)
	_, v6 := netlink.ConntrackDeleteFilters(netlink.ConntrackTable, netlink.FAMILY_V6, changed)
	return errors.Join(v4, v6)
}
