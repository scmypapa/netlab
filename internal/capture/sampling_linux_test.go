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

func TestRealContinuousOVSSampling(t *testing.T) {
	if os.Getenv("NETLAB_REAL_CAPTURE") != "1" {
		t.Skip("set NETLAB_REAL_CAPTURE=1 for native OVS test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
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
	sampler, err := NewSampler(ctx, ovs, "sample-node")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sampler != nil {
			if err := sampler.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	// Attach after sampling has started: the first frames must not wait for a polling refresh.
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
	deadline := time.Now().Add(1500 * time.Millisecond)
	var observation api.TrafficObservation
	for time.Now().Before(deadline) {
		for range 2048 {
			if err := unix.Sendto(fd, frame, 0, address); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(100 * time.Millisecond)
		observation, err = sampler.Snapshot(ctx, interfaces, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if len(observation.Flows) > 0 {
			break
		}
	}
	if len(observation.Flows) != 1 || observation.Flows[0].Bytes < int64(len(frame))*SamplingRate || observation.Flows[0].Protocol != "UDP" {
		t.Fatal("native sampling did not observe large frames", observation)
	}
	if err := sampler.Close(); err != nil {
		t.Fatal(err)
	}
	sampler = nil
	if output, err := exec.CommandContext(ctx, "ovs-vsctl", "get", "Bridge", bridge, "sflow").CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != "[]" {
		t.Fatalf("sFlow collector retained: %s %v", output, err)
	}
	sampler, err = NewSampler(ctx, ovs, "sample-node")
	if err != nil {
		t.Fatal("collector restart", err)
	}
}
