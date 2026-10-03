//go:build linux

package network

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/google/uuid"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"netlab.local/core/api"
)

func TestRealVPNDualStackLifecycle(t *testing.T) {
	if os.Getenv("NETLAB_REAL_NETWORK") != "1" {
		t.Skip("set NETLAB_REAL_NETWORK=1 on a configured OVN worker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	ovs, err := NewOVS(ctx, "unix:/run/openvswitch/db.sock", "br-int")
	if err != nil {
		t.Fatal(err)
	}
	defer ovs.Close()
	ovn, err := NewOVN(ctx, "unix:/run/ovn/ovnnb_db.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer ovn.Close()
	var roots []OpenVSwitch
	if err = ovs.client.List(ctx, &roots); err != nil || len(roots) != 1 {
		t.Fatalf("chassis: %v", err)
	}
	ovn.chassis = roots[0].ExternalIDs["system-id"]
	directory := t.TempDir()
	vpn, err := NewAccess(ctx, directory, ovs, ovn)
	if err != nil {
		t.Fatal(err)
	}
	environment := uuid.NewString()
	defer func() {
		cleanup := context.Background()
		if err := vpn.Remove(cleanup, environment); err != nil {
			t.Error(err)
		}
		if err := ovn.Remove(cleanup, environment); err != nil {
			t.Error(err)
		}
	}()
	guestID := uuid.NewString()
	guest, err := accessNamespace(guestID)
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()
	defer netns.DeleteNamed(accessName(guestID))
	gw4, gw6 := "192.168.218.1", "fd12:218::1"
	plan := api.NodePlan{EnvironmentId: environment, Spec: api.EnvironmentSpec{Networks: []api.Network{
		{Id: "v4", Name: "IPv4", Cidr: "192.168.218.0/24", Gateway: &gw4},
		{Id: "v6", Name: "IPv6", Cidr: "fd12:218::/64", Gateway: &gw6},
	}}}
	for i, config := range []struct{ id, cidr, address, mac string }{{"v4", "192.168.218.0/24", "192.168.218.10", "02:12:21:80:00:10"}, {"v6", "fd12:218::/64", "fd12:218::10", "02:12:21:80:00:11"}} {
		iface := api.Interface{Id: config.id, NetworkId: config.id, Address: config.address, Mac: config.mac, Primary: i == 0}
		asset := api.Asset{Id: config.id, Interfaces: []api.Interface{iface}}
		port := objectName("test_vpn_guest", environment, config.id)
		plan.Spec.Assets = append(plan.Spec.Assets, asset)
		plan.Assets = append(plan.Assets, api.AssetExecution{Asset: asset, Interfaces: []api.ResolvedInterface{{Id: config.id, PortName: port}}})
		host := fmt.Sprintf("nt%d%s", i, accessDevice(environment)[2:])
		if err = netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: host, MTU: 1400}, PeerName: config.id, PeerNamespace: netlink.NsFd(guest.Fd())}); err != nil {
			t.Fatal(err)
		}
		link, _ := netlink.LinkByName(host)
		if err = netlink.LinkSetUp(link); err != nil {
			t.Fatal(err)
		}
		if err = ovs.Attach(ctx, host, port, environment, config.id, "test"); err != nil {
			t.Fatal(err)
		}
		defer func(host, id string) {
			if _, err := ovs.Detach(context.Background(), host, environment, id, "test"); err != nil {
				t.Error(err)
			}
		}(host, config.id)
		if err = guest.Do(func(_ ns.NetNS) error {
			link, err := netlink.LinkByName(config.id)
			if err != nil {
				return err
			}
			mac, _ := net.ParseMAC(config.mac)
			if err = netlink.LinkSetHardwareAddr(link, mac); err != nil {
				return err
			}
			prefix := netip.MustParsePrefix(config.cidr)
			addr, _ := netlink.ParseAddr(netip.PrefixFrom(netip.MustParseAddr(config.address), prefix.Bits()).String())
			addr.Flags = unix.IFA_F_NODAD
			if err = netlink.AddrReplace(link, addr); err != nil {
				return err
			}
			return netlink.LinkSetUp(link)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err = ovn.Apply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"192.168.218.10:8087", "[fd12:218::10]:8087"} {
		var listener net.Listener
		if err = guest.Do(func(_ ns.NetNS) error { var err error; listener, err = net.Listen("tcp", address); return err }); err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		go http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "vpn-real-payload") }))
	}
	if err = vpn.Prepare(ctx, plan); err != nil {
		t.Fatal(err)
	}
	assertNoWireguard := func() {
		handle, err := ns.GetNS(filepath.Join("/run/netns", accessName(environment)))
		if err != nil {
			t.Fatal(err)
		}
		defer handle.Close()
		if err = handle.Do(func(_ ns.NetNS) error {
			_, err := netlink.LinkByName("wg0")
			var missing netlink.LinkNotFoundError
			if errors.As(err, &missing) {
				return nil
			}
			return fmt.Errorf("unexpected WireGuard interface: %v", err)
		}); err != nil {
			t.Fatal(err)
		}
	}
	checkAccess := func() {
		for _, address := range []string{"192.168.218.10", "fd12:218::10"} {
			deadline := time.Now().Add(12 * time.Second)
			for {
				connection, err := vpn.Dial(ctx, environment, netip.MustParseAddr(address), 8087)
				if err == nil {
					connection.SetDeadline(time.Now().Add(time.Second))
					fmt.Fprintf(connection, "GET / HTTP/1.1\r\nHost: test\r\nConnection: close\r\n\r\n")
					data, readErr := io.ReadAll(connection)
					connection.Close()
					if readErr == nil && containsPayload(data) {
						break
					}
					err = fmt.Errorf("guest response: %v", readErr)
				}
				if time.Now().After(deadline) {
					t.Fatalf("environment connection %s: %v", address, err)
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
	}
	assertNoWireguard()
	checkAccess()
	clientKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	peer := api.VPNPeer{Id: uuid.NewString(), Name: "dual-stack", PublicKey: clientKey.PublicKey().String(), Mode: api.Translated, Routes: []api.VPNRoute{
		{NetworkId: "v4", Cidr: "192.168.218.0/24", AccessCidr: "100.100.218.0/24"},
		{NetworkId: "v6", Cidr: "fd12:218::/64", AccessCidr: "fd99:218::/64"},
	}}
	plan.Vpn = &api.NodeVPNPlan{Peers: []api.VPNPeer{peer}}
	result, err := vpn.Apply(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.ListenPort == 0 || len(result.Peers[0].Addresses) != 2 {
		t.Fatalf("incomplete VPN result: %#v", result)
	}
	clientID := uuid.NewString()
	client, err := accessNamespace(clientID)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	defer netns.DeleteNamed(accessName(clientID))
	wg := &netlink.GenericLink{LinkAttrs: netlink.LinkAttrs{Name: "nc" + accessDevice(clientID)[2:]}, LinkType: "wireguard"}
	if err = netlink.LinkAdd(wg); err != nil {
		t.Fatal(err)
	}
	if err = netlink.LinkSetNsFd(wg, int(client.Fd())); err != nil {
		t.Fatal(err)
	}
	if err = client.Do(func(_ ns.NetNS) error {
		link, err := netlink.LinkByName(wg.Name)
		if err != nil {
			return err
		}
		if err = netlink.LinkSetMTU(link, vpnMTU); err != nil {
			return err
		}
		for _, value := range result.Peers[0].Addresses {
			addr, _ := netlink.ParseAddr(value)
			addr.Flags = unix.IFA_F_NODAD
			if err = netlink.AddrReplace(link, addr); err != nil {
				return err
			}
		}
		if err = netlink.LinkSetUp(link); err != nil {
			return err
		}
		controller, err := wgctrl.New()
		if err != nil {
			return err
		}
		defer controller.Close()
		server, _ := wgtypes.ParseKey(result.PublicKey)
		config := wgtypes.PeerConfig{PublicKey: server, Endpoint: &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: result.ListenPort}}
		for _, route := range peer.Routes {
			_, prefix, _ := net.ParseCIDR(route.AccessCidr)
			config.AllowedIPs = append(config.AllowedIPs, *prefix)
			if err = netlink.RouteReplace(&netlink.Route{LinkIndex: link.Attrs().Index, Dst: prefix}); err != nil {
				return err
			}
		}
		return controller.ConfigureDevice(wg.Name, wgtypes.Config{PrivateKey: &clientKey, Peers: []wgtypes.PeerConfig{config}})
	}); err != nil {
		t.Fatal(err)
	}
	check := func(address string) error {
		var connection net.Conn
		if err := client.Do(func(_ ns.NetNS) error {
			var err error
			connection, err = net.DialTimeout("tcp", address, time.Second)
			return err
		}); err != nil {
			return err
		}
		defer connection.Close()
		connection.SetDeadline(time.Now().Add(time.Second))
		fmt.Fprintf(connection, "GET / HTTP/1.1\r\nHost: test\r\nConnection: close\r\n\r\n")
		data, err := io.ReadAll(connection)
		if err != nil {
			return err
		}
		if !containsPayload(data) {
			return fmt.Errorf("missing real guest response")
		}
		return nil
	}
	for _, address := range []string{"100.100.218.10:8087", "[fd99:218::10]:8087"} {
		deadline := time.Now().Add(12 * time.Second)
		for {
			err = check(address)
			if err == nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("real guest %s: %v", address, err)
		}
	}
	if err = ovn.Apply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err = check("100.100.218.10:8087"); err != nil {
		t.Fatalf("VPN lost by network update: %v", err)
	}
	beforeHandshake := vpnHandshake(t, environment, clientKey.PublicKey())
	otherKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	other := peer
	other.Id, other.PublicKey = uuid.NewString(), otherKey.PublicKey().String()
	plan.Vpn.Peers = []api.VPNPeer{peer, other}
	if _, err = vpn.Apply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if after := vpnHandshake(t, environment, clientKey.PublicKey()); beforeHandshake.IsZero() || !after.Equal(beforeHandshake) {
		t.Fatal("adding another client discarded the current client's handshake")
	}
	fd, busy, err := reservePort(servicePort{protocol: unix.IPPROTO_UDP})
	if err != nil {
		t.Fatal(err)
	}
	plan.Vpn.ListenPort = int(busy.port)
	_, applyErr := vpn.Apply(ctx, plan)
	unix.Close(fd)
	if applyErr == nil {
		t.Fatal("occupied WireGuard UDP port was accepted")
	}
	if err = check("100.100.218.10:8087"); err != nil {
		t.Fatalf("failed port change did not restore active VPN: %v", err)
	}
	plan.Vpn.ListenPort = result.ListenPort
	peer.Routes = peer.Routes[:1]
	plan.Vpn.Peers = []api.VPNPeer{peer}
	if _, err = vpn.Apply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err = check("[fd99:218::10]:8087"); err == nil {
		t.Fatal("withdrawn IPv6 network remains accessible")
	}
	if err = check("100.100.218.10:8087"); err != nil {
		t.Fatalf("remaining network stopped working: %v", err)
	}
	var beforeRouter, afterRouter []Router
	if err = ovn.client.Where(&Router{Name: objectName("lr", environment, "gateway")}).List(ctx, &beforeRouter); err != nil {
		t.Fatal(err)
	}
	vpn, err = NewAccess(ctx, directory, ovs, ovn)
	if err != nil {
		t.Fatal(err)
	}
	if err = ovn.client.Where(&Router{Name: objectName("lr", environment, "gateway")}).List(ctx, &afterRouter); err != nil {
		t.Fatal(err)
	}
	if len(beforeRouter) != 1 || len(afterRouter) != 1 || !slices.Equal(beforeRouter[0].Ports, afterRouter[0].Ports) || !slices.Equal(beforeRouter[0].NAT, afterRouter[0].NAT) {
		t.Fatal("node reload rewrote persistent OVN topology")
	}
	if err = check("100.100.218.10:8087"); err != nil {
		t.Fatalf("reloading node metadata disturbed active VPN: %v", err)
	}
	plan.Vpn.Peers = nil
	withdrawn, err := vpn.Apply(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if withdrawn.ListenPort != 0 || withdrawn.PublicKey != result.PublicKey {
		t.Fatal("empty VPN did not preserve identity and release port")
	}
	assertNoWireguard()
	checkAccess()
	if err = check("100.100.218.10:8087"); err == nil {
		t.Fatal("revoked VPN still reaches the guest")
	}
	plan.Vpn.Peers = []api.VPNPeer{peer}
	restored, err := vpn.Apply(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if restored.PublicKey != result.PublicKey {
		t.Fatal("regrant changed server identity")
	}
}

func vpnHandshake(t *testing.T, environment string, public wgtypes.Key) time.Time {
	t.Helper()
	handle, err := ns.GetNS(filepath.Join("/run/netns", accessName(environment)))
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	var handshake time.Time
	if err = handle.Do(func(_ ns.NetNS) error {
		client, err := wgctrl.New()
		if err != nil {
			return err
		}
		defer client.Close()
		device, err := client.Device("wg0")
		if err != nil {
			return err
		}
		for _, peer := range device.Peers {
			if peer.PublicKey == public {
				handshake = peer.LastHandshakeTime
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return handshake
}

func containsPayload(data []byte) bool {
	return string(data[len(data)-min(len(data), len("vpn-real-payload")):]) == "vpn-real-payload"
}
