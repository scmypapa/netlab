//go:build linux

package network

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/google/uuid"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/crypto/ssh"
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
	ovn, err := NewOVN(ctx, "unix:/run/ovn/ovnnb_db.sock", nil)
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
	gateway, err := NewGateway(ctx, directory, "vpn-access-tests", "100.127.0.0/16", ovs, ovn)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	vpn, err := NewAccess(ctx, directory, ovs, ovn)
	if err != nil {
		t.Fatal(err)
	}
	environment := uuid.NewString()
	defer func() {
		cleanup := context.Background()
		if err := gateway.Remove(cleanup, environment, false); err != nil {
			t.Error(err)
		}
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
	access4, access6 := "192.168.218.254", "fd12:218::ffff"
	plan := api.NodePlan{EnvironmentId: environment, Spec: api.EnvironmentSpec{Networks: []api.Network{
		{Id: "v4", Name: "IPv4", Cidr: "192.168.218.0/24", Gateway: &gw4, AccessAddress: &access4},
		{Id: "v6", Name: "IPv6", Cidr: "fd12:218::/64", Gateway: &gw6, AccessAddress: &access6},
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
		for networkID, address := range map[string]string{"v4": "192.168.218.10", "v6": "fd12:218::10"} {
			deadline := time.Now().Add(12 * time.Second)
			for {
				connection, err := vpn.Dial(ctx, environment, networkID, netip.MustParseAddr(address), 8087)
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
	// A second independent routing domain deliberately has the same subnet and guest IP.
	duplicateID := uuid.NewString()
	duplicate, err := accessNamespace(duplicateID)
	if err != nil {
		t.Fatal(err)
	}
	defer duplicate.Close()
	defer netns.DeleteNamed(accessName(duplicateID))
	duplicateNetwork := api.Network{Id: "duplicate", Name: "Duplicate IPv4", Cidr: "192.168.218.0/24", Gateway: &gw4, AccessAddress: &access4}
	routers := *plan.Spec.WithRouting().Routers
	routers = append(routers, api.LogicalRouter{Id: "duplicate-router", Name: "Duplicate router", Interfaces: []api.RouterInterface{{Id: "duplicate-gateway", NetworkId: "duplicate", Address: gw4}}})
	plan.Spec.Routers = &routers
	plan.Spec.Networks = append(plan.Spec.Networks, duplicateNetwork)
	duplicateInterface := api.Interface{Id: "duplicate", NetworkId: "duplicate", Address: "192.168.218.10", Mac: "02:12:21:80:00:12", Primary: true}
	duplicateAsset := api.Asset{Id: "duplicate", Interfaces: []api.Interface{duplicateInterface}}
	duplicatePort := objectName("test_vpn_guest", environment, "duplicate")
	plan.Spec.Assets = append(plan.Spec.Assets, duplicateAsset)
	plan.Assets = append(plan.Assets, api.AssetExecution{Asset: duplicateAsset, Interfaces: []api.ResolvedInterface{{Id: "duplicate", PortName: duplicatePort}}})
	duplicateHost := "nd" + accessDevice(environment)[2:]
	if err = netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: duplicateHost, MTU: 1400}, PeerName: "duplicate", PeerNamespace: netlink.NsFd(duplicate.Fd())}); err != nil {
		t.Fatal(err)
	}
	duplicateLink, _ := netlink.LinkByName(duplicateHost)
	if err = netlink.LinkSetUp(duplicateLink); err != nil {
		t.Fatal(err)
	}
	if err = ovs.Attach(ctx, duplicateHost, duplicatePort, environment, "duplicate", "test"); err != nil {
		t.Fatal(err)
	}
	defer ovs.Detach(context.Background(), duplicateHost, environment, "duplicate", "test")
	if err = duplicate.Do(func(_ ns.NetNS) error {
		link, err := netlink.LinkByName("duplicate")
		if err != nil {
			return err
		}
		mac, _ := net.ParseMAC(duplicateInterface.Mac)
		if err = netlink.LinkSetHardwareAddr(link, mac); err != nil {
			return err
		}
		address, _ := netlink.ParseAddr("192.168.218.10/24")
		if err = netlink.AddrReplace(link, address); err != nil {
			return err
		}
		return netlink.LinkSetUp(link)
	}); err != nil {
		t.Fatal(err)
	}
	var duplicateListener net.Listener
	if err = duplicate.Do(func(_ ns.NetNS) error {
		var err error
		duplicateListener, err = net.Listen("tcp", "192.168.218.10:8087")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	defer duplicateListener.Close()
	go http.Serve(duplicateListener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "vpn-duplicate-payload") }))
	if err = ovn.Apply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err = vpn.Prepare(ctx, plan); err != nil {
		t.Fatal(err)
	}
	for networkID, guestNS := range map[string]ns.NetNS{"v4": guest, "duplicate": duplicate} {
		_, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		signer, err := ssh.NewSignerFromKey(private)
		if err != nil {
			t.Fatal(err)
		}
		var listener net.Listener
		if err = guestNS.Do(func(_ ns.NetNS) error {
			var err error
			listener, err = net.Listen("tcp", "192.168.218.10:2222")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		go func() {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			defer connection.Close()
			config := &ssh.ServerConfig{NoClientAuth: true}
			config.AddHostKey(signer)
			server, _, _, err := ssh.NewServerConn(connection, config)
			if err == nil {
				server.Close()
			}
		}()
		connection, err := vpn.Dial(ctx, environment, networkID, netip.MustParseAddr("192.168.218.10"), 2222)
		if err != nil {
			t.Fatal(err)
		}
		connection.SetDeadline(time.Now().Add(3 * time.Second))
		client, _, _, err := ssh.NewClientConn(connection, "192.168.218.10:2222", &ssh.ClientConfig{User: "test", HostKeyCallback: ssh.FixedHostKey(signer.PublicKey())})
		connection.Close()
		if err != nil {
			t.Fatalf("SSH selected the wrong network %s: %v", networkID, err)
		}
		client.Close()
	}
	clientKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	peer := api.VPNPeer{Id: uuid.NewString(), Name: "dual-stack", PublicKey: clientKey.PublicKey().String(), Mode: api.Translated, Routes: []api.VPNRoute{
		{NetworkId: "v4", Cidr: "192.168.218.0/24", AccessCidr: "100.100.218.0/24"},
		{NetworkId: "v6", Cidr: "fd12:218::/64", AccessCidr: "fd99:218::/64"},
		{NetworkId: "duplicate", Cidr: "192.168.218.0/24", AccessCidr: "100.100.219.0/24"},
	}}
	plan.Vpn = &api.NodeVPNPlan{Peers: []api.VPNPeer{peer}}
	result, err := vpn.Apply(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.ListenPort == 0 || len(result.Peers[0].Addresses) != 2 {
		t.Fatalf("incomplete VPN result: %#v", result)
	}
	ovn.ConfigureGateway(netip.MustParsePrefix("100.127.0.0/16"), ovn.chassis)
	plan.Gateway = &api.ServiceGateway{NodeId: "vpn-access-tests", Address: "100.127.250.2"}
	bindings := []api.NodeServiceBinding{{Id: "web", AssetId: "v4", NetworkId: "v4", Protocol: api.Tcp, TargetAddress: "192.168.218.10", TargetPort: 8087}, {Id: "duplicate", AssetId: "duplicate", NetworkId: "duplicate", Protocol: api.Tcp, TargetAddress: "192.168.218.10", TargetPort: 8087}}
	plan.Services = &bindings
	resolved, err := gateway.Apply(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, service := range resolved {
		response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d", service.ListenPort))
		if err != nil {
			t.Fatal("native service forwarding", err)
		}
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		want := "vpn-real-payload"
		if service.NetworkId == "duplicate" {
			want = "vpn-duplicate-payload"
		}
		if !strings.Contains(string(data), want) {
			t.Fatalf("service selected wrong network: %s -> %s", service.NetworkId, data)
		}
	}
	if err = ovn.Apply(ctx, plan); err != nil {
		t.Fatal("full network apply with service and VPN", err)
	}
	checkAccess()
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
		wanted := "vpn-real-payload"
		if strings.HasPrefix(address, "100.100.219.") {
			wanted = "vpn-duplicate-payload"
		}
		if !strings.Contains(string(data), wanted) {
			return fmt.Errorf("missing real guest response")
		}
		return nil
	}
	for _, address := range []string{"100.100.218.10:8087", "[fd99:218::10]:8087", "100.100.219.10:8087"} {
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
	legacy := vpn.records[environment]
	keyBefore := legacy.PrivateKey
	addressesBefore := slices.Clone(legacy.Peers[0].Addresses)
	legacy.Version = 0
	legacy.Networks = slices.Clone(legacy.Networks)
	for i := range legacy.Networks {
		legacy.Networks[i].AccessAddress = nil
	}
	if err = vpn.writeRecord(environment, legacy); err != nil {
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
	if vpn.records[environment].Version != 0 {
		t.Fatal("legacy record guessed management addresses before canonical refresh")
	}
	if err = ovn.Apply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err = vpn.Prepare(ctx, plan); err != nil {
		t.Fatal(err)
	}
	upgraded := vpn.records[environment]
	if upgraded.Version != 2 || upgraded.PrivateKey != keyBefore || upgraded.ListenPort != result.ListenPort || !slices.Equal(upgraded.Peers[0].Addresses, addressesBefore) {
		t.Fatal("canonical refresh changed VPN identity or client addresses")
	}
	deadline := time.Now().Add(12 * time.Second)
	for {
		err = check("100.100.218.10:8087")
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("canonical refresh did not restore legacy VPN: %v", err)
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
	remaining := resolved[:1]
	plan.Services = &remaining
	if _, err = gateway.Apply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	clientHTTP := &http.Client{Timeout: time.Second}
	if response, err := clientHTTP.Get(fmt.Sprintf("http://127.0.0.1:%d", resolved[1].ListenPort)); err == nil {
		response.Body.Close()
		t.Fatal("withdrawn service remains reachable")
	}
	plan.Vpn.Peers = nil
	if _, err = vpn.Apply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	plan.Spec.Networks = plan.Spec.Networks[:2]
	plan.Spec.Assets = plan.Spec.Assets[:2]
	plan.Assets = plan.Assets[:2]
	*plan.Spec.Routers = (*plan.Spec.Routers)[:1]
	if err = ovn.Apply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err = vpn.Prepare(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if connection, err := vpn.Dial(ctx, environment, "duplicate", netip.MustParseAddr("192.168.218.10"), 8087); err == nil {
		connection.Close()
		t.Fatal("removed network remains accessible")
	}
	checkAccess()
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
