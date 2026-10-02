package network

import (
	"netlab.local/core/api"
	"testing"
)

func TestPortDNSAndDefaultGateway(t *testing.T) {
	dnsAsset := "dc"
	gateway := "192.168.10.1"
	first := api.Interface{Id: "first", NetworkId: "lan", Mac: "02:00:01:02:03:04", Address: "192.168.10.10", Primary: true}
	second := api.Interface{Id: "second", NetworkId: "lan", Mac: "02:00:01:02:03:05", Address: "192.168.10.11"}
	plan := api.NodePlan{EnvironmentId: "isolation", Spec: api.EnvironmentSpec{Networks: []api.Network{{Id: "lan", Name: "LAN", Cidr: "192.168.10.0/24", Gateway: &gateway, DnsAssetId: &dnsAsset}}, Assets: []api.Asset{{Id: dnsAsset, Interfaces: []api.Interface{first}}, {Id: "member", Interfaces: []api.Interface{second}}}}, Assets: []api.AssetExecution{{Interfaces: []api.ResolvedInterface{{Id: "first", PortName: "primary-port"}, {Id: "second", PortName: "secondary-port"}}}}}
	models, err := Compile(plan)
	if err != nil {
		t.Fatal(err)
	}
	dhcp := map[string]*DHCP{}
	ports := map[string]*SwitchPort{}
	for _, m := range models {
		switch v := m.(type) {
		case *DHCP:
			dhcp[v.UUID] = v
		case *SwitchPort:
			ports[v.Name] = v
		}
	}
	for _, name := range []string{"primary-port", "secondary-port"} {
		if ports[name] == nil {
			t.Fatalf("resolved identity changed: %s", name)
		}
	}
	primary := dhcp[*ports["primary-port"].DHCPv4]
	secondary := dhcp[*ports["secondary-port"].DHCPv4]
	if primary.Options["router"] != gateway || secondary.Options["router"] != "" {
		t.Fatal("secondary NIC received a default route")
	}
	if primary.Options["dns_server"] != "{192.168.10.10}" || secondary.Options["dns_server"] != "{192.168.10.10}" {
		t.Fatal("environment DNS asset was not resolved")
	}
}
