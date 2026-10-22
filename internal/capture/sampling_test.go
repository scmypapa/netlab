package capture

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"netlab.local/core/api"
)

func sampleFrame(t testing.TB) []byte {
	return sampleFrameFrom(t, "02:00:00:00:00:01", "192.0.2.1")
}

func sampleFrameFrom(t testing.TB, mac, address string) []byte {
	t.Helper()
	source, err := net.ParseMAC(mac)
	if err != nil {
		t.Fatal(err)
	}
	ether := &layers.Ethernet{SrcMAC: source, DstMAC: net.HardwareAddr{2, 0, 0, 0, 0, 2}, EthernetType: layers.EthernetTypeIPv4}
	ip := &layers.IPv4{Version: 4, TTL: 64, SrcIP: net.ParseIP(address).To4(), DstIP: net.IPv4(192, 0, 2, 2), Protocol: layers.IPProtocolUDP}
	udp := &layers.UDP{SrcPort: 50000, DstPort: 502}
	udp.SetNetworkLayerForChecksum(ip)
	buffer := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(buffer, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}, ether, ip, udp, gopacket.Payload(make([]byte, 8000))); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func sampleDatagram(frame []byte, ingress uint32, egress ...uint32) []byte {
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
	var output uint32
	if len(egress) > 0 {
		output = egress[0]
	}
	write(&sample, 1, ingress, SamplingRate, SamplingRate, 0, ingress, output, 1)
	sample.Write(record.Bytes())
	write(&datagram, 5, 1, 0x7f000001, 0, 1, 1000, 1, 1, uint32(sample.Len()))
	datagram.Write(sample.Bytes())
	return datagram.Bytes()
}

func fixtureSampler(node string, interfaces map[uint32]string) *Sampler {
	return &Sampler{node: node, interfaces: interfaces, index: map[sampleKey]int{}}
}

func TestSampleWeightWindowIsolationAndOwnership(t *testing.T) {
	now := time.Now().UTC()
	resolved := map[uint32]string{7: "client-port", 8: "other-port"}
	s := fixtureSampler("one", resolved)
	frame := sampleFrame(t)
	for _, index := range []uint32{7, 8} {
		if err := s.ingest(sampleDatagram(frame, index), now.Add(-time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	interfaces := []api.CaptureInterface{
		{AssetId: "client", NodeId: "one", PortName: "client-port", Mac: "02:00:00:00:00:01", Address: "192.0.2.1"},
		{AssetId: "server", NodeId: "two", PortName: "server-port", Mac: "02:00:00:00:00:02", Address: "192.0.2.2"},
	}
	result, err := s.snapshot(interfaces, resolved, now)
	if err != nil || len(result.Flows) != 1 {
		t.Fatalf("snapshot %+v %v", result, err)
	}
	flow := result.Flows[0]
	if flow.Packets != SamplingRate || flow.Bytes != int64(len(frame))*SamplingRate || *flow.SourceAssetId != "client" || *flow.DestinationAssetId != "server" || flow.DestinationPort != 502 {
		t.Fatal(flow)
	}
	if unrelated, _ := s.snapshot([]api.CaptureInterface{{NodeId: "one", PortName: "missing"}}, resolved, now); len(unrelated.Flows) != 0 {
		t.Fatal("environment isolation lost", unrelated)
	}
	if expired, _ := s.snapshot(interfaces, resolved, now.Add(61*time.Second)); len(expired.Flows) != 0 {
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

func TestNewVMTapAndContainerVethRecoverSamplesWithoutPolling(t *testing.T) {
	now := time.Now().UTC()
	s := fixtureSampler("one", map[uint32]string{})
	for _, index := range []uint32{7, 8} {
		if err := s.ingest(sampleDatagram(sampleFrame(t), index), now.Add(-time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.samples) != 2 {
		t.Fatal("new ports were discarded or merged")
	}
	resolved := map[uint32]string{7: "vm-port", 8: "container-port"}
	for _, asset := range []struct{ id, port, network string }{{"windows-vm", "vm-port", "blue"}, {"linux-container", "container-port", "red"}} {
		interfaces := []api.CaptureInterface{{AssetId: asset.id, NodeId: "one", PortName: asset.port, NetworkId: asset.network, Mac: "02:00:00:00:00:01", Address: "192.0.2.1"}}
		result, err := s.snapshot(interfaces, resolved, now)
		if err != nil || len(result.Flows) != 1 || *result.Flows[0].SourceAssetId != asset.id {
			t.Fatalf("new attachment %s: %+v %v", asset.id, result, err)
		}
	}
	resolved[7] = "replacement-port"
	replacement := []api.CaptureInterface{{AssetId: "replacement", NodeId: "one", PortName: "replacement-port", NetworkId: "blue", Mac: "02:00:00:00:00:01", Address: "192.0.2.1"}}
	if result, _ := s.snapshot(replacement, resolved, now); len(result.Flows) != 0 {
		t.Fatal("old samples rebound to a replacement port", result)
	}
	if err := s.ingest(sampleDatagram(sampleFrame(t), 7), now); err != nil {
		t.Fatal(err)
	}
	if result, _ := s.snapshot(replacement, resolved, now); len(result.Flows) != 1 || result.Flows[0].Packets != SamplingRate {
		t.Fatal("replacement traffic missing or inherited old samples", result)
	}
}

func TestFlowSecondAggregationPreservesQuietEnvironmentAndTimes(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	resolved := map[uint32]string{7: "busy", 8: "quiet"}
	s := fixtureSampler("one", resolved)
	if err := s.ingest(sampleDatagram(sampleFrame(t), 8), base.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	first := base.Add(100 * time.Millisecond)
	sample := Sample{At: first, LastSeen: first, Source: "192.0.2.1", Destination: "192.0.2.2", SourceMAC: "02:00:00:00:00:01", DestinationMAC: "02:00:00:00:00:02", Protocol: "UDP", SourcePort: 50000, DestinationPort: 502, Bytes: 51200, Packets: SamplingRate, Samples: 1, PortName: "busy"}
	const count = sampleBudget * 2
	for index := range count {
		sample.At = first.Add(time.Duration(index) * time.Nanosecond)
		sample.LastSeen = sample.At
		s.add(sample, samplePort{name: "busy", ingress: 7})
	}
	if len(s.samples) != 2 || len(s.index) != 2 {
		t.Fatal("busy flow consumed packet-count capacity", len(s.samples), len(s.index))
	}
	for _, port := range []string{"busy", "quiet"} {
		result, err := s.snapshot([]api.CaptureInterface{{AssetId: port, NodeId: "one", NetworkId: port, PortName: port, Mac: "02:00:00:00:00:01", Address: "192.0.2.1"}}, resolved, base.Add(2*time.Second))
		if err != nil || len(result.Flows) != 1 || result.OmittedSamples != 0 {
			t.Fatalf("%s lost traffic: %+v %v", port, result, err)
		}
		if port == "busy" {
			flow := result.Flows[0]
			if flow.Packets != count*SamplingRate || flow.Bytes != count*sample.Bytes || !flow.FirstSeen.Equal(first) || !flow.LastSeen.Equal(sample.At) {
				t.Fatal("aggregation changed weights or exact first/last times", flow)
			}
		}
	}
}

func TestSamplingBudgetIndexExpiryAndOutOfOrderTimes(t *testing.T) {
	now := time.Now().UTC()
	s := fixtureSampler("one", map[uint32]string{7: "client"})
	sample := Sample{At: now, LastSeen: now, Source: "192.0.2.1", Destination: "192.0.2.2", SourceMAC: "02:00:00:00:00:01", Protocol: "UDP", Bytes: 100, Packets: 1, Samples: 1, PortName: "client"}
	port := samplePort{name: "client", ingress: 7}
	s.add(sample, port)
	for index := range sampleBudget + 2 {
		sample.SourcePort = index % sampleBudget
		sample.DestinationPort = index / sampleBudget
		s.add(sample, port)
	}
	if len(s.samples) != sampleBudget || len(s.index) != sampleBudget || s.omitted[now.Unix()%ObservationWindow].ports[port] != 3 {
		t.Fatal("unbounded or hidden overwrite", len(s.samples), len(s.index))
	}
	for key, index := range s.index {
		if s.samples[index].key != key {
			t.Fatal("slot overwrite left a stale index")
		}
	}
	sample.SourcePort, sample.DestinationPort = 0, 0
	s.add(sample, port)
	index := s.index[sampleKey{sample.key(), port, now.Unix()}]
	if s.samples[index].Packets != 1 {
		t.Fatal("overwritten flow merged with another slot")
	}
	if result, _ := s.snapshot([]api.CaptureInterface{{AssetId: "client", NodeId: "one", Mac: "02:00:00:00:00:01", PortName: "client"}}, s.interfaces, now.Add(61*time.Second)); len(result.Flows) != 0 || result.OmittedSamples != 0 {
		t.Fatal("expired samples or omitted records survived", result)
	}
	s = fixtureSampler("one", map[uint32]string{7: "client"})
	sample.At, sample.LastSeen = now, now
	s.add(sample, port)
	sample.At, sample.LastSeen = now.Add(-time.Nanosecond), now.Add(-time.Nanosecond)
	s.add(sample, port)
	if !s.samples[0].At.Equal(sample.At) || !s.samples[0].LastSeen.Equal(now) {
		t.Fatal("out-of-order first/last changed", s.samples[0])
	}
}

func TestFlowSecondWindowKeepsItsLatestSampleAndOmissionWeight(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	s := fixtureSampler("one", map[uint32]string{7: "client"})
	port := samplePort{name: "client", ingress: 7}
	sample := Sample{At: base.Add(100 * time.Millisecond), LastSeen: base.Add(100 * time.Millisecond), Source: "192.0.2.1", Destination: "192.0.2.2", SourceMAC: "02:00:00:00:00:01", Protocol: "UDP", Bytes: 51200, Packets: 512, Samples: 1, PortName: "client"}
	s.add(sample, port)
	sample.At, sample.LastSeen = base.Add(800*time.Millisecond), base.Add(800*time.Millisecond)
	s.add(sample, port)
	interfaces := []api.CaptureInterface{{AssetId: "client", NodeId: "one", PortName: "client", Mac: sample.SourceMAC}}
	if result, _ := s.snapshot(interfaces, s.interfaces, base.Add(60500*time.Millisecond)); len(result.Flows) != 1 || !result.Flows[0].LastSeen.Equal(sample.At) {
		t.Fatal("partially expired second lost its fresh sample", result)
	}
	if result, _ := s.snapshot(interfaces, s.interfaces, base.Add(60900*time.Millisecond)); len(result.Flows) != 0 {
		t.Fatal("fully expired second survived", result)
	}
	a := fixtureAggregate("one", "client")
	sample.PortName = ""
	for index := range flowLimit {
		sample.SourcePort = index
		a.addSample(sample)
	}
	sample.SourcePort, sample.Samples, sample.Packets = flowLimit, 17, 17*SamplingRate
	a.addSample(sample)
	if a.omitted != 17 {
		t.Fatal("flow clipping lost the number of original samples", a.omitted)
	}
}

func TestSamplingBroadcastAndCrossNodeOwnership(t *testing.T) {
	now := time.Now().UTC()
	interfaces := []api.CaptureInterface{{AssetId: "sender", NodeId: "one", PortName: "sender-port", Mac: "02:00:00:00:00:01", Address: "192.0.2.1"}, {AssetId: "receiver", NodeId: "two", PortName: "receiver-port", Mac: "02:00:00:00:00:02", Address: "192.0.2.2"}}
	one := fixtureSampler("one", map[uint32]string{7: "sender-port"})
	two := fixtureSampler("two", map[uint32]string{8: "receiver-port"})
	if err := one.ingest(sampleDatagram(sampleFrame(t), 7), now); err != nil {
		t.Fatal(err)
	}
	if err := two.ingest(sampleDatagram(sampleFrame(t), 999, 8), now); err != nil {
		t.Fatal(err)
	}
	first, _ := one.snapshot(interfaces, one.interfaces, now)
	second, _ := two.snapshot(interfaces, two.interfaces, now)
	if len(first.Flows) != 1 || len(second.Flows) != 0 {
		t.Fatal("cross-node duplicate", first, second)
	}
	if err := two.ingest(sampleDatagram(sampleFrameFrom(t, "00:11:22:33:44:55", "203.0.113.10"), 999, 8), now); err != nil {
		t.Fatal(err)
	}
	second, _ = two.snapshot(interfaces, two.interfaces, now)
	if len(second.Flows) != 1 || second.Flows[0].SourceAssetId != nil || *second.Flows[0].DestinationAssetId != "receiver" {
		t.Fatal("external inbound lost", second)
	}
	ether := &layers.Ethernet{SrcMAC: net.HardwareAddr{2, 0, 0, 0, 0, 1}, DstMAC: net.HardwareAddr{255, 255, 255, 255, 255, 255}, EthernetType: layers.EthernetTypeARP}
	arp := &layers.ARP{AddrType: layers.LinkTypeEthernet, Protocol: layers.EthernetTypeIPv4, HwAddressSize: 6, ProtAddressSize: 4, Operation: 1, SourceHwAddress: ether.SrcMAC, SourceProtAddress: []byte{192, 0, 2, 1}, DstHwAddress: make([]byte, 6), DstProtAddress: []byte{192, 0, 2, 2}}
	buffer := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(buffer, gopacket.SerializeOptions{FixLengths: true}, ether, arp); err != nil {
		t.Fatal(err)
	}
	if err := one.ingest(sampleDatagram(buffer.Bytes(), 7, 2<<30|2), now); err != nil {
		t.Fatal(err)
	}
	first, _ = one.snapshot(interfaces, one.interfaces, now)
	if len(first.Flows) != 2 {
		t.Fatal("broadcast with multiple output interfaces lost", first)
	}
}

func BenchmarkSamplerRepeatedFlow(b *testing.B) {
	s := fixtureSampler("one", nil)
	port := samplePort{name: "port", ingress: 7}
	now := time.Now()
	sample := Sample{At: now, LastSeen: now, Source: "192.0.2.1", Destination: "192.0.2.2", Protocol: "UDP", Bytes: 51200, Packets: 512, Samples: 1, PortName: "port"}
	b.ReportAllocs()
	for range b.N {
		s.add(sample, port)
	}
	b.ReportMetric(float64(len(s.samples)), "slots")
}

func BenchmarkSamplerIngestRepeatedFlow(b *testing.B) {
	s := fixtureSampler("one", map[uint32]string{7: "port"})
	packet := sampleDatagram(sampleFrame(b), 7)
	now := time.Now().UTC()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := s.ingest(packet, now); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(s.samples)), "slots")
}

func BenchmarkSamplerDistinctFlows(b *testing.B) {
	s := fixtureSampler("one", nil)
	port := samplePort{name: "port", ingress: 7}
	now := time.Now().Truncate(time.Second)
	sample := Sample{Source: "192.0.2.1", Destination: "192.0.2.2", Protocol: "UDP", Bytes: 51200, Packets: 512, Samples: 1, PortName: "port"}
	b.ReportAllocs()
	for index := range b.N {
		sample.SourcePort = index % sampleBudget
		sample.At = now.Add(time.Duration(index/sampleBudget) * time.Second)
		sample.LastSeen = sample.At
		s.add(sample, port)
	}
	b.ReportMetric(float64(len(s.samples)), "slots")
}
