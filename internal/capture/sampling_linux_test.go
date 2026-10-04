//go:build linux

package capture

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"netlab.local/core/api"
	"netlab.local/core/internal/network"
)

func sampleFrame(t *testing.T) []byte {
	t.Helper()
	ether := &layers.Ethernet{SrcMAC: net.HardwareAddr{2, 0, 0, 0, 0, 1}, DstMAC: net.HardwareAddr{2, 0, 0, 0, 0, 2}, EthernetType: layers.EthernetTypeIPv4}
	ip := &layers.IPv4{Version: 4, TTL: 64, SrcIP: net.IPv4(192, 0, 2, 1), DstIP: net.IPv4(192, 0, 2, 2), Protocol: layers.IPProtocolUDP}
	udp := &layers.UDP{SrcPort: 50000, DstPort: 502}
	udp.SetNetworkLayerForChecksum(ip)
	buffer := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(buffer, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}, ether, ip, udp, gopacket.Payload(make([]byte, 8000))); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func sampleDatagram(frame []byte, index uint32) []byte {
	var raw, record, sample, datagram bytes.Buffer
	write := func(buffer *bytes.Buffer, values ...uint32) { binary.Write(buffer, binary.BigEndian, values) }
	header := frame[:min(len(frame), 128)]
	write(&raw, 1, uint32(len(frame)), 0, uint32(len(header)))
	raw.Write(header)
	for raw.Len()%4 != 0 {
		raw.WriteByte(0)
	}
	write(&record, 1, uint32(raw.Len()))
	record.Write(raw.Bytes())
	write(&sample, 1, index, SamplingRate, SamplingRate, 0, index, 0, 1)
	sample.Write(record.Bytes())
	write(&datagram, 5, 1, 0x7f000001, 0, 1, 1000, 1, 1, uint32(sample.Len()))
	datagram.Write(sample.Bytes())
	return datagram.Bytes()
}

func TestSampleWeightWindowIsolationAndOwnership(t *testing.T) {
	now := time.Now().UTC()
	s := &Sampler{node: "one", interfaces: map[uint32]string{7: "client-port", 8: "other-port"}, omitted: map[string][ObservationWindow]sampleDrop{}}
	packet := sampleDatagram(sampleFrame(t), 7)
	if err := s.ingest(packet, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.ingest(sampleDatagram(sampleFrame(t), 8), now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	interfaces := []api.CaptureInterface{
		{AssetId: "client", NodeId: "one", PortName: "client-port", Mac: "02:00:00:00:00:01", Address: "192.0.2.1"},
		{AssetId: "server", NodeId: "two", PortName: "server-port", Mac: "02:00:00:00:00:02", Address: "192.0.2.2"},
	}
	result, err := s.Snapshot(interfaces, now)
	if err != nil || len(result.Flows) != 1 {
		t.Fatalf("snapshot %+v %v", result, err)
	}
	flow := result.Flows[0]
	if flow.Packets != SamplingRate || flow.Bytes != int64(len(sampleFrame(t)))*SamplingRate || *flow.SourceAssetId != "client" || *flow.DestinationAssetId != "server" || flow.DestinationPort != 502 {
		t.Fatal(flow)
	}
	if unrelated, _ := s.Snapshot([]api.CaptureInterface{{NodeId: "one", PortName: "missing"}}, now); len(unrelated.Flows) != 0 {
		t.Fatal("environment isolation lost", unrelated)
	}
	if expired, _ := s.Snapshot(interfaces, now.Add(61*time.Second)); len(expired.Flows) != 0 {
		t.Fatal("expired samples retained", expired)
	}
	if duplicate, _ := Summarize("two", interfaces, []Sample{{At: now, Source: "192.0.2.1", Destination: "192.0.2.2", SourceMAC: "02:00:00:00:00:01", DestinationMAC: "02:00:00:00:00:02", Packets: 512, Bytes: 8192}}, now); len(duplicate) != 0 {
		t.Fatal("cross-node sample double counted", duplicate)
	}
	if err := s.ingest([]byte{0, 1, 2}, now); err == nil {
		t.Fatal("invalid datagram accepted")
	}
	if len(s.samples) != 2 {
		t.Fatal("invalid datagram changed buffer")
	}
}

func TestSamplingBufferBoundAndOutOfOrderRate(t *testing.T) {
	now := time.Now().UTC()
	s := &Sampler{node: "one", interfaces: map[uint32]string{7: "client-port"}, omitted: map[string][ObservationWindow]sampleDrop{}}
	packet := sampleDatagram(sampleFrame(t), 7)
	for range sampleBudget + 2 {
		if err := s.ingest(packet, now); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.samples) != sampleBudget || s.omitted["client-port"][now.Unix()%ObservationWindow].count != 2 {
		t.Fatal("unbounded or hidden overwrite", len(s.samples), s.omitted)
	}
	interfaces := []api.CaptureInterface{{AssetId: "client", NodeId: "one", Mac: "02:00:00:00:00:01", PortName: "client-port"}}
	sample := Sample{At: now.Add(-time.Second), Source: "192.0.2.1", Destination: "192.0.2.2", SourceMAC: "02:00:00:00:00:01", Protocol: "UDP", Bytes: 100, Packets: 1}
	old := sample
	old.At = now.Add(-11 * time.Second)
	flows, _ := Summarize("one", interfaces, []Sample{sample, old}, now)
	if len(flows) != 1 || flows[0].BytesPerSecond <= 0 || !flows[0].LastSeen.Equal(sample.At) || !flows[0].FirstSeen.Equal(old.At) {
		t.Fatal("ring order changed rate or times", flows)
	}
}

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
	command("add-port", bridge, device, "--", "set", "Interface", device, "external_ids:iface-id=sample-client")
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
	interfaces := []api.CaptureInterface{{AssetId: "client", NodeId: "sample-node", PortName: "sample-client", Mac: "02:00:00:00:00:01"}}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(unix.ETH_P_ALL)))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	output, _ := netlink.LinkByName(peer)
	address := &unix.SockaddrLinklayer{Ifindex: output.Attrs().Index, Protocol: htons(unix.ETH_P_ALL)}
	frame := sampleFrame(t)
	deadline := time.Now().Add(10 * time.Second)
	var observation api.TrafficObservation
	for time.Now().Before(deadline) {
		for range 2048 {
			if err := unix.Sendto(fd, frame, 0, address); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(100 * time.Millisecond)
		observation, err = sampler.Snapshot(interfaces, time.Now().UTC())
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
