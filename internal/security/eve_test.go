package security

import (
	"fmt"
	"netlab.local/core/api"
	"testing"
)

func peer(env, asset, device, node string) api.SecurityInterface {
	return api.SecurityInterface{EnvironmentId: env, Interface: api.CaptureInterface{AssetId: asset, AssetName: asset,
		InterfaceId: device, PortName: device, NetworkId: "lan", NodeId: node, Address: "10.0.0.2", Mac: "02:00:00:00:00:02"}}
}

func eve(device, source, destination string) []byte {
	return []byte(fmt.Sprintf(`{"timestamp":"2026-10-22T12:23:34.123456+0800","event_type":"alert","in_iface":%q,"src_ip":%q,"dest_ip":%q,"proto":"TCP","src_port":40000,"dest_port":80,"alert":{"gid":1,"signature_id":10001,"rev":1,"severity":1,"category":"Web Application Attack","signature":"Test detection"}}`, device, source, destination))
}

func TestEVEOverlappingEnvironmentsUseCaptureDevice(t *testing.T) {
	blue, red := peer("blue", "web", "tap-blue", "one"), peer("red", "web", "tap-red", "one")
	devices := []attachment{{Device: "tap-blue", SecurityInterface: blue}, {Device: "tap-red", SecurityInterface: red}}
	decoder := newEVEDecoder("one", devices, []api.SecurityInterface{blue, red})
	for _, device := range devices {
		_, alert, err := decoder.decode(eve(device.Device, "10.0.0.2", "10.0.0.3"))
		if err != nil {
			t.Fatal(err)
		}
		if alert == nil || alert.EnvironmentId != device.EnvironmentId || alert.SourceAssetId != "web" || alert.FirstSeen.Hour() != 12 || alert.Count != 1 {
			t.Fatalf("wrong scope or native timestamp: %+v", alert)
		}
	}
}

func TestEVEDuplicateEndpointsAndExternalIngress(t *testing.T) {
	source, target := peer("env", "client", "client-tap", "one"), peer("env", "server", "server-tap", "two")
	target.Interface.Address = "10.0.0.3"
	peers := []api.SecurityInterface{source, target}
	cases := []struct {
		node, device, src, dst string
		keep                   bool
	}{
		{"one", "client-tap", "10.0.0.2", "10.0.0.3", true},
		{"two", "server-tap", "10.0.0.2", "10.0.0.3", false},
		{"two", "server-tap", "198.51.100.1", "10.0.0.3", true},
	}
	devices := []attachment{{Device: "client-tap", SecurityInterface: source}, {Device: "server-tap", SecurityInterface: target}}
	for _, test := range cases {
		_, alert, err := newEVEDecoder(test.node, devices, peers).decode(eve(test.device, test.src, test.dst))
		if err != nil {
			t.Fatal(err)
		}
		if (alert != nil) != test.keep {
			t.Fatalf("%+v: %+v", test, alert)
		}
	}
}

func TestEVEStatsAndUnknownDevice(t *testing.T) {
	decoder := newEVEDecoder("one", nil, nil)
	event, alert, err := decoder.decode([]byte(`{"event_type":"stats","stats":{"capture":{"kernel_packets":120,"kernel_drops":3}}}`))
	if err != nil || alert != nil || event.Stats.Capture.Packets != 120 || event.Stats.Capture.Drops != 3 {
		t.Fatalf("%+v %v", event, err)
	}
	if _, _, err = decoder.decode(eve("unknown", "10.0.0.2", "10.0.0.3")); err == nil {
		t.Fatal("unknown interface was accepted")
	}
}
