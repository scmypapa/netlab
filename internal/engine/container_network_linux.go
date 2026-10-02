//go:build linux

package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
	"netlab.local/core/api"
	"netlab.local/core/internal/network"
)

func deviceName(port string) string { return "nl" + strings.ReplaceAll(port, "-", "")[:12] }
func (c *Containers) connect(ctx context.Context, pid uint32, env string, a api.AssetExecution) error {
	nsHandle, err := ns.GetNS(fmt.Sprintf("/proc/%d/ns/net", pid))
	if err != nil {
		return err
	}
	defer nsHandle.Close()
	for index, i := range a.Interfaces {
		hostName := deviceName(i.PortName)
		guestName := fmt.Sprintf("eth%d", index)
		_, err := netlink.LinkByName(hostName)
		var missing netlink.LinkNotFoundError
		if errors.As(err, &missing) {
			link := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: hostName, MTU: i.Mtu}, PeerName: "p" + hostName[1:], PeerNamespace: netlink.NsFd(nsHandle.Fd())}
			if err = netlink.LinkAdd(link); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		host, err := netlink.LinkByName(hostName)
		if err != nil {
			return err
		}
		if err = netlink.LinkSetUp(host); err != nil {
			return err
		}
		if err = c.ovs.Attach(ctx, hostName, i.PortName, env, a.Asset.Id, a.InstanceId); err != nil {
			return err
		}
		if err = nsHandle.Do(func(_ ns.NetNS) error {
			peer, err := netlink.LinkByName(guestName)
			if errors.As(err, &missing) {
				peer, err = netlink.LinkByName("p" + hostName[1:])
				if err != nil {
					return err
				}
				if err = netlink.LinkSetName(peer, guestName); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			mac, err := net.ParseMAC(i.Mac)
			if err != nil {
				return err
			}
			if err = netlink.LinkSetHardwareAddr(peer, mac); err != nil {
				return err
			}
			if err = netlink.LinkSetMTU(peer, i.Mtu); err != nil {
				return err
			}
			addr, err := netlink.ParseAddr(fmt.Sprintf("%s/%d", i.Address, i.Prefix))
			if err != nil {
				return err
			}
			if err = netlink.AddrReplace(peer, addr); err != nil {
				return err
			}
			if err = netlink.LinkSetUp(peer); err != nil {
				return err
			}
			lo, err := netlink.LinkByName("lo")
			if err != nil {
				return err
			}
			if err = netlink.LinkSetUp(lo); err != nil {
				return err
			}
			if i.Primary && i.Gateway != nil && *i.Gateway != "" {
				gateway := net.ParseIP(*i.Gateway)
				if gateway == nil {
					return fmt.Errorf("invalid gateway")
				}
				if err = netlink.RouteReplace(&netlink.Route{LinkIndex: peer.Attrs().Index, Gw: gateway}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}
func (c *Containers) disconnect(ctx context.Context, a api.AssetExecution) error {
	var errs []error
	for _, i := range a.Interfaces {
		name := deviceName(i.PortName)
		if err := network.ClearShape(name, i.PortName); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := c.ovs.Detach(ctx, name, a.InstanceId); err != nil {
			errs = append(errs, err)
			continue
		}
		link, err := netlink.LinkByName(name)
		var missing netlink.LinkNotFoundError
		if errors.As(err, &missing) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err = netlink.LinkDel(link); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
func writeNetworkFiles(directory string, interfaces []api.ResolvedInterface) error {
	dns := []string{}
	seen := make(map[string]bool)
	for _, i := range interfaces {
		if i.Dns != nil {
			for _, d := range *i.Dns {
				if !seen[d] {
					seen[d] = true
					dns = append(dns, "nameserver "+d)
				}
			}
		}
	}
	hosts := "127.0.0.1 localhost\n::1 localhost ip6-localhost\n"
	if err := os.WriteFile(filepath.Join(directory, "resolv.conf"), []byte(strings.Join(dns, "\n")+"\n"), 0644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, "hosts"), []byte(hosts), 0644)
}
