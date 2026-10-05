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
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"netlab.local/core/api"
)

type Access struct {
	mu        sync.RWMutex
	directory string
	ovs       *OVS
	ovn       *OVN
	records   map[string]accessRecord
}

func NewAccess(ctx context.Context, directory string, ovs *OVS, ovn *OVN) (*Access, error) {
	v := &Access{directory: filepath.Join(directory, "access"), ovs: ovs, ovn: ovn, records: map[string]accessRecord{}}
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
		var record accessRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, err
		}
		v.records[entry.Name()[:len(entry.Name())-5]] = record
	}
	ovn.accessRecord = func(environment string) *accessRecord {
		v.mu.RLock()
		defer v.mu.RUnlock()
		record, exists := v.records[environment]
		if !exists {
			return nil
		}
		return &record
	}
	for environment, record := range v.records {
		// OVN topology persists independently; startup restores only host resources.
		if err := v.applyKernel(ctx, environment, &record, record); err != nil {
			return nil, fmt.Errorf("restore environment access %s: %w", environment, err)
		}
		if err := v.writeRecord(environment, record); err != nil {
			return nil, err
		}
		v.records[environment] = record
	}
	return v, nil
}

func (v *Access) writeRecord(environment string, record accessRecord) error {
	file, err := os.CreateTemp(v.directory, ".access-")
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

func (v *Access) Apply(ctx context.Context, plan api.NodePlan) (api.NodeVPNResult, error) {
	if plan.Vpn == nil {
		return api.NodeVPNResult{}, fmt.Errorf("VPN application requires the complete peer set")
	}
	v.mu.RLock()
	previous, exists := v.records[plan.EnvironmentId]
	v.mu.RUnlock()
	if !exists {
		key, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			return api.NodeVPNResult{}, err
		}
		transit, clients, err := vpnRanges(plan)
		if err != nil {
			return api.NodeVPNResult{}, err
		}
		previous = accessRecord{PrivateKey: key.String(), Transit: transit, Clients: clients, Networks: slices.Clone(plan.Spec.Networks)}
		// The server key belongs to the environment, including while it has no peers.
		if err := v.writeRecord(plan.EnvironmentId, previous); err != nil {
			return api.NodeVPNResult{}, err
		}
		v.mu.Lock()
		v.records[plan.EnvironmentId] = previous
		v.mu.Unlock()
	}
	next, err := accessNetwork(previous, plan)
	if err != nil {
		return api.NodeVPNResult{}, err
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

func (v *Access) apply(ctx context.Context, environment string, record *accessRecord, previous accessRecord) error {
	if err := v.applyKernel(ctx, environment, record, previous); err != nil {
		return err
	}
	return v.ovn.applyAccess(ctx, environment, record)
}

func (v *Access) applyKernel(ctx context.Context, environment string, record *accessRecord, previous accessRecord) error {
	handle, err := accessNamespace(environment)
	if err != nil {
		return err
	}
	defer handle.Close()
	if err = v.connect(ctx, environment, handle, record); err != nil {
		return err
	}
	if err = v.configureWireguard(environment, handle, record); err != nil {
		return err
	}
	if err = handle.Do(func(_ ns.NetNS) error { return applyAccessFilter(record) }); err != nil {
		return err
	}
	if len(record.Peers) == 0 {
		record.ListenPort = 0
		return nil
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

func accessNamespace(environment string) (ns.NetNS, error) {
	path := filepath.Join("/run/netns", accessName(environment))
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
	created, err := netns.NewNamed(accessName(environment))
	restore := netns.Set(current)
	if created.IsOpen() {
		created.Close()
	}
	if err = errors.Join(err, restore); err != nil {
		return nil, err
	}
	return ns.GetNS(path)
}

func (v *Access) connect(ctx context.Context, environment string, handle ns.NetNS, record *accessRecord) error {
	var missing netlink.LinkNotFoundError
	hostName := accessDevice(environment)
	host, err := netlink.LinkByName(hostName)
	if errors.As(err, &missing) {
		link := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: hostName, MTU: 1400}, PeerName: "access0", PeerNamespace: netlink.NsFd(handle.Fd())}
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
	if err = v.ovs.Attach(ctx, hostName, accessPort(environment), environment, "access", "access"); err != nil {
		return err
	}
	return handle.Do(func(_ ns.NetNS) error {
		peer, err := netlink.LinkByName("access0")
		if err != nil {
			return err
		}
		address := netip.MustParsePrefix(record.Transit[0]).Addr().Next().Next()
		mac, _ := net.ParseMAC(routerMAC(address))
		if err = netlink.LinkSetHardwareAddr(peer, mac); err != nil {
			return err
		}
		if err = replaceAccessAddresses(peer, record.Transit, 2); err != nil {
			return err
		}
		if err = netlink.LinkSetUp(peer); err != nil {
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

func (v *Access) configureWireguard(environment string, handle ns.NetNS, record *accessRecord) error {
	hostName := accessDevice(environment)
	var missing netlink.LinkNotFoundError
	var err error
	if len(record.Peers) == 0 {
		return handle.Do(func(_ ns.NetNS) error {
			link, err := netlink.LinkByName("wg0")
			if errors.As(err, &missing) {
				return nil
			}
			if err != nil {
				return err
			}
			return netlink.LinkDel(link)
		})
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
		if err = replaceAccessAddresses(wg, record.Clients, 1); err != nil {
			return err
		}
		if err = netlink.LinkSetUp(wg); err != nil {
			return err
		}
		return nil
	})
}

func (v *Access) removeKernel(ctx context.Context, environment string) error {
	if _, err := os.Stat(filepath.Join("/run/netns", accessName(environment))); err == nil {
		// Removing the dedicated namespace releases its conntrack table with it.
		if err := netns.DeleteNamed(accessName(environment)); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := v.ovs.Detach(ctx, accessDevice(environment), environment, "access", "access"); err != nil {
		return err
	}
	link, err := netlink.LinkByName(accessDevice(environment))
	var missing netlink.LinkNotFoundError
	if errors.As(err, &missing) {
		return nil
	}
	if err != nil {
		return err
	}
	return netlink.LinkDel(link)
}

func (v *Access) Remove(ctx context.Context, environment string) error {
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

type vpnConnections map[netip.Addr]bool

func (filter vpnConnections) MatchConntrackFlow(flow *netlink.ConntrackFlow) bool {
	address, _ := netip.AddrFromSlice(flow.Forward.SrcIP)
	return filter[address.Unmap()]
}

func clearChangedVPNConnections(previous, next accessRecord) error {
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
