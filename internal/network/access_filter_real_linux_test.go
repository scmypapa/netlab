//go:build linux

package network

import (
	"fmt"
	"os"
	"testing"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/google/nftables"
	"github.com/google/uuid"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"netlab.local/core/api"
)

func TestRealAccessFilterLargeBatch(t *testing.T) {
	if os.Getenv("NETLAB_REAL_NETWORK") != "1" {
		t.Skip("set NETLAB_REAL_NETWORK=1 with network namespace privileges")
	}
	for _, scenario := range []struct{ networks, peers, services int }{{11, 0, 0}, {64, 0, 0}, {11, 16, 64}} {
		t.Run(fmt.Sprintf("networks%d_peers%d_services%d", scenario.networks, scenario.peers, scenario.services), func(t *testing.T) {
			environment := uuid.NewString()
			handle, err := accessNamespace(environment)
			if err != nil {
				t.Fatal(err)
			}
			defer netns.DeleteNamed(accessName(environment))
			defer handle.Close()
			err = handle.Do(func(_ ns.NetNS) error {
				record := &accessRecord{}
				for i := range scenario.networks {
					address := fmt.Sprintf("10.%d.0.254", i)
					network := api.Network{Id: fmt.Sprint(i), Name: fmt.Sprint(i), Cidr: fmt.Sprintf("10.%d.0.0/24", i), AccessAddress: &address}
					device := &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vr" + accessLinkID(environment, network.Id)}, Table: uint32(1000 + i)}
					if err := netlink.LinkAdd(device); err != nil {
						return err
					}
					record.Networks = append(record.Networks, network)
				}
				for i := range scenario.peers {
					peer := vpnPeer{Addresses: []string{fmt.Sprintf("10.251.0.%d/32", i+1)}}
					for index, network := range record.Networks {
						peer.Peer.Routes = append(peer.Peer.Routes, api.VPNRoute{NetworkId: network.Id, Cidr: network.Cidr, AccessCidr: fmt.Sprintf("10.200.%d.0/24", index)})
					}
					record.Peers = append(record.Peers, peer)
				}
				services := gatewayRecord{}
				for i := range scenario.services {
					services.Services = append(services.Services, api.NodeServiceBinding{Id: fmt.Sprint(i), NetworkId: "0", Protocol: api.Tcp, TargetAddress: "10.0.0.10", TargetPort: 80, ListenPort: 30000 + i})
				}
				for range 2 {
					if err := applyAccessFilter(environment, record, services); err != nil {
						return err
					}
				}
				conn := &nftables.Conn{}
				table := &nftables.Table{Name: "netlab_vpn", Family: nftables.TableFamilyINet}
				rules, err := conn.GetRules(table, &nftables.Chain{Name: "forward", Table: table})
				if err != nil {
					return err
				}
				want := scenario.networks*4 + scenario.networks*scenario.peers + scenario.services
				if len(rules) != want {
					return fmt.Errorf("forward rules: got %d, want %d", len(rules), want)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
