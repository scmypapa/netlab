//go:build linux

package network

import (
	"context"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"netlab.local/core/api"
)

func TestDiscoveryPacketAndScope(t *testing.T) {
	mac, _ := net.ParseMAC("02:00:00:00:01:20")
	address := netip.MustParseAddr("192.0.2.20")
	prefix := netip.MustParsePrefix("192.0.2.0/24")
	vlan := 123
	for _, tag := range []*int{nil, &vlan} {
		raw, err := discoveryRequest(mac, address, tag)
		if err != nil {
			t.Fatal(err)
		}
		packet := gopacket.NewPacket(raw, layers.LayerTypeEthernet, gopacket.Default)
		arp := packet.Layer(layers.LayerTypeARP).(*layers.ARP)
		if arp.Operation != layers.ARPRequest || !net.IP(arp.SourceProtAddress).Equal(net.IPv4zero) {
			t.Fatal("discovery changed sender IP", arp)
		}
		arp.Operation, arp.SourceProtAddress = layers.ARPReply, address.AsSlice()
		parts := []gopacket.SerializableLayer{packet.Layer(layers.LayerTypeEthernet).(*layers.Ethernet)}
		if tag != nil {
			parts = append(parts, packet.Layer(layers.LayerTypeDot1Q).(*layers.Dot1Q))
		}
		buffer := gopacket.NewSerializeBuffer()
		if err := gopacket.SerializeLayers(buffer, gopacket.SerializeOptions{FixLengths: true}, append(parts, arp)...); err != nil {
			t.Fatal(err)
		}
		device, ok := discoveryReply(buffer.Bytes(), prefix, tag)
		if !ok || device.Address != address.String() || device.Mac != mac.String() || device.ObservedAt.IsZero() {
			t.Fatal("lost real discovery response", device, ok)
		}
		if _, ok := discoveryReply(buffer.Bytes(), netip.MustParsePrefix("198.51.100.0/24"), tag); ok {
			t.Fatal("accepted another network")
		}
		wrong := 456
		if _, ok := discoveryReply(buffer.Bytes(), prefix, &wrong); ok {
			t.Fatal("accepted another VLAN")
		}
	}
	for _, cidr := range []string{"0.0.0.0/24", "10.0.0.0/8", "fd00::/64", "bad"} {
		if _, err := discoveryPrefix(api.ExternalDiscovery{Interface: "eth1", Cidr: cidr}); err == nil {
			t.Fatalf("accepted discovery scope %s", cidr)
		}
	}
}

func TestDiscoveryRealLAN(t *testing.T) {
	if os.Getenv("NETLAB_TEST_EXTERNAL_DISCOVERY") != "1" {
		t.Skip("NETLAB_TEST_EXTERNAL_DISCOVERY=1 and root required")
	}
	name := "nld" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	ip := func(args ...string) {
		t.Helper()
		if raw, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("ip %v: %s: %v", args, raw, err)
		}
	}
	ip("netns", "add", name)
	defer exec.Command("ip", "netns", "del", name).Run()
	ip("netns", "exec", name, "sysctl", "-qw", "net.ipv4.conf.all.arp_ignore=1")
	ip("link", "add", name, "type", "veth", "peer", "name", "peer", "netns", name)
	defer exec.Command("ip", "link", "del", name).Run()
	ip("link", "set", name, "up")
	ip("-n", name, "link", "set", "peer", "up")
	ip("-n", name, "addr", "add", "192.0.2.20/24", "dev", "peer")
	ip("-n", name, "link", "add", "link", "peer", "name", "tagged", "type", "vlan", "id", "123")
	ip("-n", name, "link", "set", "tagged", "up")
	ip("-n", name, "addr", "add", "192.0.2.21/24", "dev", "tagged")
	macLink, err := exec.Command("ip", "-n", name, "-j", "link", "show", "peer").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Log("isolated physical-device peer", string(macLink))
	for _, tag := range []int{0, 123} {
		input := api.ExternalDiscovery{Interface: name, Cidr: "192.0.2.0/24"}
		want := "192.0.2.20"
		if tag != 0 {
			input.Vlan, want = &tag, "192.0.2.21"
		}
		iface, err := net.InterfaceByName(name)
		if err != nil {
			t.Fatal(err)
		}
		devices, err := discoverARP(context.Background(), input, netip.MustParsePrefix(input.Cidr), iface.HardwareAddr)
		if err != nil || len(devices) != 1 || devices[0].Address != want {
			t.Fatalf("VLAN %d: devices=%+v err=%v", tag, devices, err)
		}
		t.Logf("VLAN %d: %+v", tag, devices)
	}
	iface, _ := net.InterfaceByName(name)
	addresses, err := iface.Addrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range addresses {
		if strings.HasPrefix(address.String(), "192.0.2.") {
			t.Fatal("discovery assigned an address", address)
		}
	}
}
