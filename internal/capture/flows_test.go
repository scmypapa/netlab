package capture

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"netlab.local/core/api"
)

func fixtureAggregate(node string, selected ...string) *aggregate {
	return newAggregate(node, api.NodeCaptureRequest{Settings: api.CreateCapture{AssetIds: selected}, Interfaces: []api.CaptureInterface{
		{AssetId: "client", NodeId: "one", Mac: "02:00:00:00:00:01", Address: "192.0.2.1/24"},
		{AssetId: "server", NodeId: "two", Mac: "02:00:00:00:00:02", Address: "192.0.2.2/24"},
	}})
}

func TestConversationOwnershipAndDirection(t *testing.T) {
	packet := strings.Join([]string{"1791115200.500", "90", "02:00:00:00:00:01", "02:00:00:00:00:02", "192.0.2.1", "192.0.2.2", "", "", "", "", "50000", "502", "Modbus/TCP"}, "\t")
	one, two := fixtureAggregate("one", "client", "server"), fixtureAggregate("two", "client", "server")
	for range 3 {
		if err := one.add(packet); err != nil {
			t.Fatal(err)
		}
		if err := two.add(packet); err != nil {
			t.Fatal(err)
		}
	}
	flows := one.snapshot()
	if len(flows) != 1 || len(two.snapshot()) != 0 {
		t.Fatalf("duplicate observations: %+v %+v", flows, two.snapshot())
	}
	flow := flows[0]
	if *flow.SourceAssetId != "client" || *flow.DestinationAssetId != "server" || flow.Bytes != 270 || flow.Packets != 3 || flow.DestinationPort != 502 {
		t.Fatal(flow)
	}
	onlyReceiver := fixtureAggregate("two", "server")
	if err := onlyReceiver.add(packet); err != nil || len(onlyReceiver.snapshot()) != 1 {
		t.Fatal("receiver capture lost inbound packet", err)
	}
}

func TestExternalAndIPv6Packets(t *testing.T) {
	a := fixtureAggregate("one", "client")
	packet := strings.Join([]string{"1791115200", "120", "02:00:00:00:00:01", "00:11:22:33:44:55", "", "", "2001:db8::1", "2001:db8::2", "50100", "443", "", "", "TLSv1.3"}, "\t")
	if err := a.add(packet); err != nil {
		t.Fatal(err)
	}
	flow := a.snapshot()[0]
	if flow.Source != "2001:db8::1" || flow.DestinationAssetId != nil || *flow.SourceAssetId != "client" || flow.DestinationPort != 443 {
		t.Fatal(flow)
	}
	if err := a.add("broken field stream"); err == nil {
		t.Fatal("malformed decoder output accepted")
	}
}

func TestAggregationBudgetDoesNotAlterPacketCounters(t *testing.T) {
	a := fixtureAggregate("one", "client")
	for index := range flowLimit + 2 {
		packet := strings.Join([]string{"1791115200", "100", "02:00:00:00:00:01", "00:11:22:33:44:55", "192.0.2.1", "198.51.100.1", "", "", fmt.Sprint(index), "443", "", "", "TCP"}, "\t")
		if err := a.add(packet); err != nil {
			t.Fatal(err)
		}
	}
	if len(a.flows) != flowLimit || a.omitted != 2 || a.packets != flowLimit+2 || a.bytes != 100*(flowLimit+2) {
		t.Fatalf("flows=%d omitted=%d packets=%d bytes=%d", len(a.flows), a.omitted, a.packets, a.bytes)
	}
}
func TestRateWindowAndOverlappingAddresses(t *testing.T) {
	a := fixtureAggregate("one", "client")
	base := time.Unix(1791115200, 0)
	a.started = base
	packet := strings.Join([]string{"1791115200.500", "100", "02:00:00:00:00:01", "02:00:00:00:00:02", "192.0.2.1", "192.0.2.2", "", "", "", "", "50000", "9000", "UDP"}, "\t")
	if err := a.add(packet); err != nil {
		t.Fatal(err)
	}
	if rate := a.snapshotAt(base.Add(5 * time.Second))[0].BytesPerSecond; rate != 20 {
		t.Fatal("incorrect live rate", rate)
	}
	if rate := a.snapshotAt(base.Add(11 * time.Second))[0].BytesPerSecond; rate != 0 {
		t.Fatal("idle flow did not decay", rate)
	}
	a.addresses["192.0.2.1"] = api.CaptureInterface{}
	if iface, ok := a.endpoint("192.0.2.1", "02:00:00:00:00:01"); !ok || iface.AssetId != "client" {
		t.Fatal("MAC identity lost", iface, ok)
	}
	if _, ok := a.endpoint("192.0.2.1", "00:aa:bb:cc:dd:ee"); ok {
		t.Fatal("ambiguous address attributed to an asset")
	}
}
