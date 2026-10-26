//go:build linux

package network

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"netlab.local/core/api"
)

// Prepare shares the environment's access link with SSH, SFTP and WireGuard.
func (v *Access) Prepare(ctx context.Context, plan api.NodePlan) error {
	if v.ovn.upgradeServices != nil {
		if err := v.ovn.upgradeServices(plan.EnvironmentId, plan.Spec); err != nil {
			return err
		}
	}
	v.mu.RLock()
	current := v.records[plan.EnvironmentId]
	v.mu.RUnlock()
	peers := make([]api.VPNPeer, len(current.Peers))
	available := map[string]bool{}
	for _, network := range plan.Spec.Networks {
		available[network.Id] = network.AccessAddress != nil
	}
	for index, peer := range current.Peers {
		peers[index] = peer.Peer
		peers[index].Routes = slices.DeleteFunc(slices.Clone(peer.Peer.Routes), func(route api.VPNRoute) bool { return !available[route.NetworkId] })
	}
	plan.Vpn = &api.NodeVPNPlan{Peers: peers, ListenPort: current.ListenPort}
	_, err := v.Apply(ctx, plan)
	return err
}

func (v *Access) Dial(ctx context.Context, environment, networkID string, target netip.Addr, port uint16) (net.Conn, error) {
	v.mu.RLock()
	record, exists := v.records[environment]
	v.mu.RUnlock()
	allowed := false
	for _, network := range record.Networks {
		if network.Id == networkID && network.AccessAddress != nil && netip.MustParsePrefix(network.Cidr).Contains(target) {
			allowed = true
			break
		}
	}
	if !exists || !allowed || port == 0 {
		return nil, fmt.Errorf("target is outside the environment access context")
	}
	handle, err := ns.GetNS(filepath.Join("/run/netns", accessName(environment)))
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	var connection net.Conn
	err = handle.Do(func(_ ns.NetNS) error {
		vrfName := "vr" + accessLinkID(environment, networkID)
		vrf, err := netlink.LinkByName(vrfName)
		if err != nil {
			return err
		}
		dialer := net.Dialer{Control: func(_, _ string, raw syscall.RawConn) error {
			var controlErr error
			err := raw.Control(func(fd uintptr) {
				controlErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, vrfName)
				if controlErr == nil {
					controlErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, int(vrf.(*netlink.Vrf).Table))
				}
			})
			if err != nil {
				return err
			}
			return controlErr
		}}
		connection, err = dialer.DialContext(ctx, "tcp", net.JoinHostPort(target.String(), strconv.Itoa(int(port))))
		return err
	})
	if err != nil && connection != nil {
		connection.Close()
	}
	return connection, err
}

func replaceAccessAddresses(link netlink.Link, prefixes []string, endpoint int) error {
	wanted := map[string]*netlink.Addr{}
	for _, value := range prefixes {
		prefix := netip.MustParsePrefix(value)
		address := prefix.Addr()
		for step := 0; step < endpoint; step++ {
			address = address.Next()
		}
		item, err := netlink.ParseAddr(netip.PrefixFrom(address, prefix.Bits()).String())
		if err != nil {
			return err
		}
		item.Flags = unix.IFA_F_NODAD
		wanted[item.IPNet.String()] = item
	}
	current, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return err
	}
	for _, address := range current {
		if !address.IP.IsLinkLocalUnicast() && wanted[address.IPNet.String()] == nil {
			if err = netlink.AddrDel(link, &address); err != nil {
				return err
			}
		}
	}
	for _, address := range wanted {
		if err = netlink.AddrReplace(link, address); err != nil {
			return err
		}
	}
	return nil
}
