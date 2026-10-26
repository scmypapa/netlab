//go:build linux

package network

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/google/nftables"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"netlab.local/core/api"
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

func TestGatewayIndependentEnvironmentCommit(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "rollback"
		}
		t.Run(name, func(t *testing.T) {
			inGatewayNamespace(t, func() {
				handle, err := ns.GetCurrentNS()
				if err != nil {
					t.Fatal(err)
				}
				defer handle.Close()
				gateway := &Gateway{
					directory: t.TempDir(), table: "gateway_concurrent", address: netip.MustParseAddr("100.127.0.1"),
					records: map[string]gatewayRecord{}, sockets: map[servicePort]portReservation{},
				}
				defer gateway.Close()
				entered := make(chan int, 1)
				release := make(chan struct{})
				defer close(release)
				failure := errors.New("injected Access failure")
				gateway.ovn = &OVN{applyServices: func(ctx context.Context, plan api.NodePlan, _ gatewayRecord) error {
					if plan.EnvironmentId == "slow" && len(*plan.Services) > 0 {
						entered <- (*plan.Services)[0].ListenPort
						select {
						case <-release:
						case <-ctx.Done():
							return ctx.Err()
						}
						if fail {
							return failure
						}
					}
					return nil
				}}
				plan := func(environment, address string) api.NodePlan {
					services := []api.NodeServiceBinding{{Id: "web", NetworkId: "network", Protocol: api.Tcp, TargetAddress: "192.0.2.10", TargetPort: 80}}
					return api.NodePlan{EnvironmentId: environment, Gateway: &api.ServiceGateway{Address: address}, Services: &services}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				slow := make(chan error, 1)
				go func() {
					slow <- handle.Do(func(_ ns.NetNS) error {
						_, err := gateway.Apply(ctx, plan("slow", "100.127.0.2"))
						return err
					})
				}()
				var port int
				select {
				case port = <-entered:
				case err := <-slow:
					t.Fatalf("slow operation did not enter Access: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				fast := make(chan error, 1)
				go func() {
					fast <- handle.Do(func(_ ns.NetNS) error {
						_, err := gateway.Apply(ctx, plan("fast", "100.127.0.3"))
						return err
					})
				}()
				select {
				case err := <-fast:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("unrelated environment waited for slow Access work")
				}
				release <- struct{}{}
				if err := <-slow; (!fail && err != nil) || (fail && !errors.Is(err, failure)) {
					t.Fatalf("slow result: %v", err)
				}
				want := 2
				if fail {
					want = 1
					fd, _, err := reservePort(servicePort{protocol: unix.IPPROTO_TCP, port: uint16(port)})
					if err != nil {
						t.Fatalf("failed operation retained its host port: %v", err)
					}
					unix.Close(fd)
				}
				if len(gateway.records) != want || len(gateway.sockets) != want || len(gateway.records["fast"].Services) != 1 {
					t.Fatalf("concurrent commit lost a record or port: records=%v sockets=%v", gateway.records, gateway.sockets)
				}
				c := &nftables.Conn{}
				set, err := c.GetSetByName(&nftables.Table{Name: gateway.table, Family: nftables.TableFamilyIPv4}, "ports")
				if err != nil {
					t.Fatal(err)
				}
				values, err := c.GetSetElements(set)
				if err != nil || len(values) != want {
					t.Fatalf("concurrent commit lost native NAT bindings: %v, %v", values, err)
				}
				for environment, record := range gateway.records {
					data, err := os.ReadFile(filepath.Join(gateway.directory, environment+".json"))
					if err != nil {
						t.Fatal(err)
					}
					var stored gatewayRecord
					if err := json.Unmarshal(data, &stored); err != nil || stored.Gateway != record.Gateway || len(stored.Services) != 1 {
						t.Fatalf("persisted concurrent state: %+v, %v", stored, err)
					}
				}
			})
		})
	}
}
