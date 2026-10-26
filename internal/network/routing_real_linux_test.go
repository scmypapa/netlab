//go:build linux

package network

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/google/uuid"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"netlab.local/core/api"
)

// Real dataplane coverage: three routing hops, return paths, overlapping isolated
// subnets, policy updates, stable port identities and complete OVN removal.
func TestRealRoutedTopology(t *testing.T) {
	if os.Getenv("NETLAB_REAL_NETWORK") != "1" {
		t.Skip("set NETLAB_REAL_NETWORK=1 on a configured OVN worker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ovs, err := NewOVS(ctx, "unix:/run/openvswitch/db.sock", "br-int")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ovs.Close)
	ovn, err := NewOVN(ctx, "unix:/run/ovn/ovnnb_db.sock", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ovn.Close)
	environment := uuid.NewString()
	t.Cleanup(func() {
		if err := ovn.Remove(context.Background(), environment); err != nil {
			t.Error(err)
		}
	})
	plan := api.NodePlan{EnvironmentId: environment}
	for _, config := range []struct{ id, cidr string }{{"a", "10.241.1.0/24"}, {"c", "10.241.3.0/24"}, {"isolated", "10.241.1.0/24"}, {"t1", "172.28.240.0/30"}, {"t2", "172.28.241.0/30"}} {
		plan.Spec.Networks = append(plan.Spec.Networks, api.Network{Id: config.id, Name: config.id, Cidr: config.cidr})
	}
	plan.Spec.Routers = &[]api.LogicalRouter{
		{Id: "r1", Name: "R1", Interfaces: []api.RouterInterface{{Id: "r1a", NetworkId: "a", Address: "10.241.1.1"}, {Id: "r1t", NetworkId: "t1", Address: "172.28.240.1"}}},
		{Id: "r2", Name: "R2", Interfaces: []api.RouterInterface{{Id: "r2t1", NetworkId: "t1", Address: "172.28.240.2"}, {Id: "r2t2", NetworkId: "t2", Address: "172.28.241.1"}}},
		{Id: "r3", Name: "R3", Interfaces: []api.RouterInterface{{Id: "r3t", NetworkId: "t2", Address: "172.28.241.2"}, {Id: "r3c", NetworkId: "c", Address: "10.241.3.1"}}},
		{Id: "r4", Name: "Isolated", Interfaces: []api.RouterInterface{{Id: "r4lan", NetworkId: "isolated", Address: "10.241.1.1"}}},
	}
	routes := []api.Route{}
	for _, config := range []struct{ router, network, destination, next string }{{"r1", "t1", "10.241.3.0/24", "172.28.240.2"}, {"r2", "t1", "10.241.1.0/24", "172.28.240.1"}, {"r2", "t2", "10.241.3.0/24", "172.28.241.2"}, {"r3", "t2", "10.241.1.0/24", "172.28.241.1"}} {
		id := config.router
		routes = append(routes, api.Route{RouterId: &id, NetworkId: config.network, Destination: config.destination, NextHop: config.next})
	}
	plan.Spec.Routes = &routes
	guests := map[string]ns.NetNS{}
	for i, config := range []struct{ network, address, gateway string }{{"a", "10.241.1.10", "10.241.1.1"}, {"c", "10.241.3.10", "10.241.3.1"}, {"isolated", "10.241.1.20", "10.241.1.1"}} {
		id := uuid.NewString()
		guest, err := accessNamespace(id)
		if err != nil {
			t.Fatal(err)
		}
		guests[config.network] = guest
		t.Cleanup(func() { guest.Close(); netns.DeleteNamed(accessName(id)) })
		device := fmt.Sprintf("nrt%d%s", i, strings.ReplaceAll(environment, "-", "")[:8])
		portName := objectName("routing_test", environment, config.network)
		mac := fmt.Sprintf("02:fa:ce:00:00:%02x", i+1)
		iface := api.Interface{Id: config.network, NetworkId: config.network, Address: config.address, Mac: mac, Primary: true}
		asset := api.Asset{Id: config.network, Name: config.network, Interfaces: []api.Interface{iface}}
		plan.Spec.Assets = append(plan.Spec.Assets, asset)
		plan.Assets = append(plan.Assets, api.AssetExecution{Asset: asset, Interfaces: []api.ResolvedInterface{{Id: iface.Id, PortName: portName}}})
		if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: device, MTU: 1400}, PeerName: "eth0", PeerNamespace: netlink.NsFd(guest.Fd())}); err != nil {
			t.Fatal(err)
		}
		host, err := netlink.LinkByName(device)
		if err != nil {
			t.Fatal(err)
		}
		if err := netlink.LinkSetUp(host); err != nil {
			t.Fatal(err)
		}
		if err := ovs.Attach(ctx, device, portName, environment, config.network, "routing_test"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := ovs.Detach(context.Background(), device, environment, config.network, "routing_test"); err != nil {
				t.Error(err)
			}
		})
		var listener net.Listener
		if err := guest.Do(func(_ ns.NetNS) error {
			link, err := netlink.LinkByName("eth0")
			if err != nil {
				return err
			}
			hardware, _ := net.ParseMAC(mac)
			if err := netlink.LinkSetHardwareAddr(link, hardware); err != nil {
				return err
			}
			address, _ := netlink.ParseAddr(config.address + "/24")
			if err := netlink.AddrAdd(link, address); err != nil {
				return err
			}
			if err := netlink.LinkSetUp(link); err != nil {
				return err
			}
			if err := netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: net.ParseIP(config.gateway)}); err != nil {
				return err
			}
			listener, err = net.Listen("tcp4", net.JoinHostPort(config.address, "18080"))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close() })
		go func(name string) {
			for {
				connection, err := listener.Accept()
				if err != nil {
					return
				}
				io.WriteString(connection, name)
				connection.Close()
			}
		}(config.network)
	}
	read := func(source, destination string) (string, error) {
		var connection net.Conn
		err := guests[source].Do(func(_ ns.NetNS) error {
			var err error
			connection, err = net.DialTimeout("tcp4", net.JoinHostPort(destination, "18080"), 250*time.Millisecond)
			return err
		})
		if err != nil {
			return "", err
		}
		defer connection.Close()
		connection.SetReadDeadline(time.Now().Add(time.Second))
		data, err := io.ReadAll(connection)
		return string(data), err
	}
	waitRead := func(source, destination, want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			got, err := read(source, destination)
			if err == nil && got == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s → %s: got %q, error %v", source, destination, got, err)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	if err := ovn.Apply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	waitRead("a", "10.241.3.10", "c")
	waitRead("c", "10.241.1.10", "a")
	if got, err := read("a", "10.241.1.20"); err == nil {
		t.Fatalf("overlapping isolated switch leaked into routed domain: %q", got)
	}
	// Recompile the entire topology without recreating the guest ports.
	if err := ovn.Apply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	waitRead("a", "10.241.3.10", "c")
	protocol := "tcp"
	plan.Spec.Policies = &[]api.Policy{{Id: "block", NetworkId: "c", Action: api.Deny, Direction: api.Ingress, Protocol: &protocol}}
	if err := ovn.Apply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := read("a", "10.241.3.10"); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("updated network policy did not reach dataplane")
		}
		time.Sleep(25 * time.Millisecond)
	}
	plan.Spec.Policies = nil
	if err := ovn.Apply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	waitRead("a", "10.241.3.10", "c")
}
