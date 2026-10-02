//go:build linux

package network

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"runtime"
	"testing"

	"github.com/google/nftables"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func inGatewayNamespace(t *testing.T, run func()) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires a root-owned Linux network namespace")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	original, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	created, err := netns.New()
	if err != nil {
		t.Fatal(err)
	}
	defer created.Close()
	defer func() {
		if err := netns.Set(original); err != nil {
			t.Fatal(err)
		}
	}()
	run()
}

func TestServicePortKernelReservation(t *testing.T) {
	inGatewayNamespace(t, func() {
		for _, protocol := range []uint8{unix.IPPROTO_TCP, unix.IPPROTO_UDP} {
			fd, port, err := reservePort(servicePort{protocol: protocol})
			if err != nil {
				t.Fatal(err)
			}
			if port.port == 0 {
				t.Fatal("kernel did not allocate a real port")
			}
			if next, _, err := reservePort(port); !errors.Is(err, unix.EADDRINUSE) {
				unix.Close(next)
				t.Fatalf("occupied port was accepted: %v", err)
			}
			unix.Close(fd)
			next, same, err := reservePort(port)
			if err != nil || same != port {
				t.Fatalf("released port was not reusable: %v", err)
			}
			unix.Close(next)
		}
	})
}

func TestServiceKernelMap(t *testing.T) {
	inGatewayNamespace(t, func() {
		c := &nftables.Conn{}
		c.AddTable(&nftables.Table{Name: "unrelated", Family: nftables.TableFamilyIPv4})
		if err := c.Flush(); err != nil {
			t.Fatal(err)
		}
		gateway, bridge := netip.MustParseAddr("100.127.0.2"), netip.MustParseAddr("100.127.0.1")
		bindings := []kernelBinding{{port: servicePort{protocol: unix.IPPROTO_TCP, port: 24443}, address: gateway}, {port: servicePort{protocol: unix.IPPROTO_UDP, port: 24443}, address: gateway}}
		for _, count := range []int{2, 1, 0} {
			if err := applyKernelBindings("gateway_test", bridge, bindings[:count], []netip.Addr{gateway}); err != nil {
				t.Fatalf("apply %d native bindings: %v", count, err)
			}
			set, err := c.GetSetByName(&nftables.Table{Name: "gateway_test", Family: nftables.TableFamilyIPv4}, "ports")
			if err != nil {
				t.Fatal(err)
			}
			values, err := c.GetSetElements(set)
			if err != nil || len(values) != count {
				t.Fatalf("native map retained withdrawn bindings: count=%d err=%v", len(values), err)
			}
		}
		tables, err := c.ListTables()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, table := range tables {
			found = found || table.Name == "unrelated"
		}
		if !found {
			t.Fatal("native update deleted unrelated firewall state")
		}
	})
}

func TestServiceConntrackFilter(t *testing.T) {
	local := netip.MustParseAddr("10.0.0.10")
	gateway := netip.MustParseAddr("100.127.0.2")
	filter := bindingConnections{gateway: gateway, ports: map[servicePort]bool{{protocol: unix.IPPROTO_TCP, port: 24443}: true}, local: map[netip.Addr]bool{local: true}}
	for _, test := range []struct {
		destination string
		zone        uint16
		port        uint16
		match       bool
	}{
		{gateway.String(), 7, 24443, true}, {local.String(), 0, 24443, true},
		{local.String(), 7, 24443, false}, {gateway.String(), 7, 443, false},
		{"10.0.0.99", 0, 24443, false},
	} {
		flow := &netlink.ConntrackFlow{Zone: test.zone, Forward: netlink.IPTuple{Protocol: unix.IPPROTO_TCP, DstIP: net.ParseIP(test.destination), DstPort: test.port}}
		if filter.MatchConntrackFlow(flow) != test.match {
			t.Fatalf("conntrack ownership for %s:%d zone %d", test.destination, test.port, test.zone)
		}
	}
}
