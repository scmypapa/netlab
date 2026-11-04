//go:build linux

package capture

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"netlab.local/core/api"
	"netlab.local/core/internal/network"
)

func TestRealContinuousOVSFlows(t *testing.T) {
	if os.Getenv("NETLAB_REAL_CAPTURE") != "1" {
		t.Skip("set NETLAB_REAL_CAPTURE=1 for native OVS test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	bridge, device, peer := "sf"+suffix, "si"+suffix, "sp"+suffix
	command := func(args ...string) {
		t.Helper()
		if output, err := exec.CommandContext(ctx, "ovs-vsctl", args...).CombinedOutput(); err != nil {
			t.Fatalf("OVS %v %s", err, output)
		}
	}
	command("add-br", bridge, "--", "set", "Bridge", bridge, "fail_mode=standalone")
	t.Cleanup(func() { exec.Command("ovs-vsctl", "--if-exists", "del-br", bridge).Run() })
	command("--", "--id=@old", "create", "sFlow", "targets=\"127.0.0.1:9\"", "sampling=512", "external_ids:netlab.node=sample-node", "--", "set", "Bridge", bridge, "sflow=@old")
	link := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: device}, PeerName: peer}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { netlink.LinkDel(link) })
	for _, name := range []string{device, peer} {
		l, err := netlink.LinkByName(name)
		if err != nil {
			t.Fatal(err)
		}
		if err = netlink.LinkSetUp(l); err != nil {
			t.Fatal(err)
		}
		if err = netlink.LinkSetMTU(l, 65535); err != nil {
			t.Fatal(err)
		}
	}
	ovs, err := network.NewOVS(ctx, "unix:/var/run/openvswitch/db.sock", bridge)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ovs.Close)
	collector, err := NewCollector(ctx, ovs, "sample-node")
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, "ovs-vsctl", "get", "Bridge", bridge, "sflow").CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != "[]" {
		t.Fatalf("old exporter retained: %s %v", output, err)
	}
	t.Cleanup(func() {
		if collector != nil {
			if err := collector.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	// Attach after flow export has started: the first frames must not wait for a polling refresh.
	command("add-port", bridge, device, "--", "set", "Interface", device, "external_ids:iface-id=sample-client")
	interfaces := []api.CaptureInterface{{AssetId: "client", NodeId: "sample-node", PortName: "sample-client", Mac: "02:00:00:00:00:01"}}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(unix.ETH_P_ALL)))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	output, _ := netlink.LinkByName(peer)
	address := &unix.SockaddrLinklayer{Ifindex: output.Attrs().Index, Protocol: htons(unix.ETH_P_ALL)}
	frame := sampleFrame(t)
	for attempt := range 3 {
		if err := unix.Sendto(fd, frame, 0, address); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		var observed *api.CaptureFlow
		for time.Now().Before(deadline) && observed == nil {
			time.Sleep(100 * time.Millisecond)
			observation, err := collector.Snapshot(ctx, interfaces, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			for _, flow := range observation.Flows {
				if flow.Protocol == "UDP" && flow.SourcePort == 50000 && flow.DestinationPort == 502 {
					observed = &flow
					break
				}
			}
		}
		if observed == nil || observed.Packets != 1 || observed.Bytes < int64(len(frame)) {
			collector.mu.Lock()
			t.Logf("collector port=%s interfaces=%v samples=%+v err=%v", collector.conn.LocalAddr(), collector.interfaces, collector.samples, collector.err)
			collector.mu.Unlock()
			for _, args := range [][]string{{"ovs-ofctl", "dump-ipfix-bridge", bridge}, {"ovs-appctl", "dpif/dump-flows", bridge}, {"cat", "/proc/net/udp"}, {"cat", "/proc/net/snmp"}} {
				output, err := exec.Command(args[0], args[1:]...).CombinedOutput()
				t.Logf("%v: %s %v", args, output, err)
			}
			t.Fatalf("single packet on collector start %d: %+v", attempt, observed)
		}
		if attempt == 1 {
			// A process exit leaves the OVS exporter pointing at the old UDP receiver.
			collector.conn.Close()
			collector.workers.Wait()
		} else {
			if err := collector.Close(); err != nil {
				t.Fatal(err)
			}
			if output, err := exec.CommandContext(ctx, "ovs-vsctl", "get", "Bridge", bridge, "ipfix").CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != "[]" {
				t.Fatalf("IPFIX collector retained: %s %v", output, err)
			}
		}
		collector = nil
		if attempt < 2 {
			collector, err = NewCollector(ctx, ovs, "sample-node")
			if err != nil {
				t.Fatal("collector restart", err)
			}
		}
	}
}
