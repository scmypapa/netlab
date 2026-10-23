//go:build linux

package capture

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcapgo"
	"netlab.local/core/api"
)

func TestRealCompressedRecentPackets(t *testing.T) {
	if os.Getenv("NETLAB_REAL_CAPTURE") != "1" {
		t.Skip("set NETLAB_REAL_CAPTURE=1 for native TShark test")
	}
	r, environment, interfaces := recordingFixture(t, 1<<20)
	frame := sampleFrame(t)
	at := time.Unix(1791115200, 0)
	for index := 0; index < 3; index++ {
		if err := r.Record(environment, interfaces, 0, gopacket.CaptureInfo{Timestamp: at.Add(time.Duration(index) * time.Millisecond), Length: len(frame), CaptureLength: len(frame)}, frame); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	page, err := r.Recent(ctx, environment, RecentQuery{Limit: 2, Filter: "udp.dstport == 502", Network: "lan"})
	if err != nil || len(page.Packets) != 2 || page.NextCursor == nil {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	if page.Packets[0].Number != 2 || page.Packets[1].Number != 3 {
		t.Fatal("latest page is not chronological", page)
	}
	older, err := r.Recent(ctx, environment, RecentQuery{Limit: 2, Cursor: *page.NextCursor})
	if err != nil || len(older.Packets) != 1 || older.Packets[0].Number != 1 || older.NextCursor != nil {
		t.Fatalf("older=%+v err=%v", older, err)
	}
	detail, err := r.RecentPacket(ctx, environment, *page.Packets[0].Id)
	if err != nil || detail.Hex != hex.EncodeToString(frame) || len(detail.Layers) < 3 {
		t.Fatalf("detail=%+v err=%v", detail, err)
	}
	statistics := pcapgo.DefaultNgInterfaceStatistics
	statistics.PacketsDropped, statistics.PacketsReceived = 7, 10
	if err = r.interfaceStatistics(environment, interfaces[0].PortName, statistics); err != nil {
		t.Fatal(err)
	}
	var exported bytes.Buffer
	if err = r.Export(ctx, environment, RecentQuery{Filter: "udp.dstport == 502"}, &exported); err != nil {
		t.Fatal(err)
	}
	exportedData := append([]byte{}, exported.Bytes()...)
	count := 0
	if err = readFrames(ctx, &exported, false, func(api.CapturePacket, []byte) bool { count++; return true }); err != nil || count != 3 {
		t.Fatalf("export count=%d err=%v", count, err)
	}
	var exportedDrops *uint64
	reader, err := pcapgo.NewNgReader(bytes.NewReader(exportedData), pcapgo.NgReaderOptions{StatisticsCallback: func(index int, stats pcapgo.NgInterfaceStatistics) {
		value := stats.PacketsDropped
		exportedDrops = &value
	}})
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, _, err = reader.ReadPacketData(); err != nil {
			break
		}
	}
	if exportedDrops == nil || *exportedDrops != 7 {
		t.Fatalf("native interface drop statistics stripped from filtered export: %v", exportedDrops)
	}
	if _, err = r.Recent(ctx, environment, RecentQuery{Limit: 100, Filter: "invalid_filter_x ===="}); err == nil {
		t.Fatal("invalid display filter accepted")
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = r.RecentPacket(ctx, environment, *page.Packets[0].Id); err != nil {
		t.Fatal("sealed part identity changed", err)
	}
	packet := gopacket.NewPacket(frame, layers.LayerTypeEthernet, gopacket.Default)
	ip := packet.Layer(layers.LayerTypeIPv4).(*layers.IPv4)
	udp := packet.Layer(layers.LayerTypeUDP).(*layers.UDP)
	udp.DstPort = 53
	udp.SetNetworkLayerForChecksum(ip)
	dns := &layers.DNS{ID: 7, RD: true, Questions: []layers.DNSQuestion{{Name: []byte("example.test"), Type: layers.DNSTypeA, Class: layers.DNSClassIN}}}
	buffer := gopacket.NewSerializeBuffer()
	if err = gopacket.SerializeLayers(buffer, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}, packet.Layer(layers.LayerTypeEthernet).(*layers.Ethernet), ip, udp, dns); err != nil {
		t.Fatal(err)
	}
	if err = r.Record(environment, interfaces, 0, gopacket.CaptureInfo{Timestamp: at.Add(time.Second), Length: len(buffer.Bytes()), CaptureLength: len(buffer.Bytes())}, buffer.Bytes()); err != nil {
		t.Fatal(err)
	}
	page, err = r.Recent(ctx, environment, RecentQuery{Limit: 10, Protocol: "DNS"})
	if err != nil || len(page.Packets) != 1 || page.Packets[0].Protocol != "DNS" || page.Packets[0].TransportProtocol == nil || *page.Packets[0].TransportProtocol != "UDP" {
		t.Fatalf("application and transport protocols: %+v %v", page, err)
	}
	detail, err = r.RecentPacket(ctx, environment, *page.Packets[0].Id)
	if err != nil || detail.Packet.TransportProtocol == nil || *detail.Packet.TransportProtocol != "UDP" {
		t.Fatalf("detail transport protocol: %+v %v", detail, err)
	}
}
