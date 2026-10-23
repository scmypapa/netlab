package capture

import (
	"bytes"
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcapgo"
	"netlab.local/core/api"
)

func packetFixture(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	w, err := pcapgo.NewNgWriter(&data, layers.LinkTypeEthernet)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.AddInterface(pcapgo.NgInterface{LinkType: layers.LinkTypeEthernet, SnapLength: 65535}); err != nil {
		t.Fatal(err)
	}
	for index, frame := range [][]byte{sampleFrame(t), sampleARPFrame(t, "02:00:00:00:00:01"), sampleFrame(t)} {
		if err = w.WritePacket(gopacket.CaptureInfo{Timestamp: time.Unix(1791115200, int64(index)*1000000), Length: len(frame), CaptureLength: len(frame), InterfaceIndex: index % 2}, frame); err != nil {
			t.Fatal(err)
		}
	}
	if err = w.Flush(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestPacketReadAndConnectionFilter(t *testing.T) {
	q, err := ParsePacketQuery(url.Values{"source": {"192.0.2.2"}, "destination": {"192.0.2.1"}, "sourcePort": {"502"}, "destinationPort": {"50000"}, "protocol": {"udp"}, "limit": {"1"}})
	if err != nil {
		t.Fatal(err)
	}
	var packets []api.CapturePacket
	err = readFrames(context.Background(), bytes.NewReader(packetFixture(t)), false, func(packet api.CapturePacket, _ []byte) bool {
		packets = append(packets, packet)
		return true
	})
	if err != nil || len(packets) != 3 {
		t.Fatalf("packets=%+v error=%v", packets, err)
	}
	if !q.matches(packets[0]) || q.matches(packets[1]) || packets[1].InterfaceIndex != 1 || packets[1].Summary != "192.0.2.1 → 192.0.2.2" {
		t.Fatal(packets)
	}
	if packets[2].Number != 3 || packets[0].CapturedLength != packets[0].Length || packets[0].Protocol != "UDP" || packets[0].TransportProtocol == nil || *packets[0].TransportProtocol != "UDP" || packets[1].TransportProtocol != nil {
		t.Fatal(packets)
	}
	for _, values := range []url.Values{{"after": {"-1"}}, {"limit": {"201"}}, {"sourcePort": {"65536"}}, {"destinationPort": {"text"}}, {"interfaceIndex": {"-1"}}} {
		if _, err := ParsePacketQuery(values); err == nil {
			t.Fatal("invalid query accepted", values)
		}
	}
}

func TestPacketInterfaceSeparatesOverlappingAddresses(t *testing.T) {
	q, err := ParsePacketQuery(url.Values{"source": {"192.0.2.1"}, "destination": {"192.0.2.2"}, "interfaceIndex": {"1"}})
	if err != nil {
		t.Fatal(err)
	}
	frame := sampleFrame(t)
	for _, index := range []int{0, 1} {
		packet := summarizePacket(1, frame, gopacket.CaptureInfo{InterfaceIndex: index}, layers.LinkTypeEthernet)
		if q.matches(packet) != (index == 1) {
			t.Fatal("identical addresses mixed between capture interfaces", packet)
		}
	}
}

func TestPacketReadWhileCaptureIsWriting(t *testing.T) {
	data := packetFixture(t)
	for _, live := range []bool{true, false} {
		count := 0
		err := readFrames(context.Background(), bytes.NewReader(data[:len(data)-5]), live, func(api.CapturePacket, []byte) bool { count++; return true })
		if err != nil || count != 2 {
			t.Fatalf("live=%v count=%d err=%v", live, count, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := readFrames(ctx, bytes.NewReader(data), false, func(api.CapturePacket, []byte) bool { t.Fatal("cancelled read continued"); return true }); err != context.Canceled {
		t.Fatal(err)
	}
}
