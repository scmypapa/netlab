package network

import (
	"encoding/json"
	"netlab.local/core/api"
	"strings"
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

func TestExternalGatewayUsesResolvedChassis(t *testing.T) {
	pool, gateway := "fd11::100/120", "fd11::1"
	vlan := 200
	chassis := map[string]string{"node": "actual-ovs-chassis"}
	plan := api.NodePlan{EnvironmentId: "external", ExternalChassis: &chassis, Spec: api.EnvironmentSpec{Networks: []api.Network{{Id: "lan", Name: "LAN", Cidr: "fd11::/64", Gateway: &gateway, AllocationPool: &pool, External: &api.ExternalAttachment{NodeId: "node", Interface: "eth1", Vlan: &vlan}}}}}
	models, err := Compile(plan)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range models {
		switch v := item.(type) {
		case *SwitchPort:
			if v.Type == "l2gateway" {
				found = true
				if v.Options["l2gateway-chassis"] != "actual-ovs-chassis" || v.TagRequest == nil || *v.TagRequest != 200 {
					t.Fatalf("wrong gateway binding: %+v", v)
				}
			}
		case *RouterPort:
			if len(v.IPv6RA) != 0 || len(v.Networks) != 1 || v.Networks[0] != "fd11::101/64" {
				t.Fatalf("external LAN gateway or RA changed: %+v", v)
			}
		}
	}
	if !found {
		t.Fatal("external gateway not compiled")
	}
}

func TestCompileIndependentRoutingTables(t *testing.T) {
	var spec api.EnvironmentSpec
	if err := json.Unmarshal([]byte(`{"networks":[{"id":"left","name":"Left","cidr":"10.0.0.0/24"},{"id":"right","name":"Right","cidr":"10.0.0.0/24"},{"id":"transit","name":"Transit","cidr":"172.20.0.0/30"}],"routers":[{"id":"r1","name":"R1","interfaces":[{"id":"p1","networkId":"left","address":"10.0.0.1"},{"id":"p2","networkId":"transit","address":"172.20.0.1"}]},{"id":"r2","name":"R2","interfaces":[{"id":"p3","networkId":"right","address":"10.0.0.1"},{"id":"p4","networkId":"transit","address":"172.20.0.2"}]}],"routes":[{"routerId":"r1","networkId":"transit","destination":"192.0.2.0/24","nextHop":"172.20.0.2"},{"routerId":"r2","networkId":"transit","destination":"198.51.100.0/24","nextHop":"172.20.0.1"}]}`), &spec); err != nil {
		t.Fatal(err)
	}
	models, err := Compile(api.NodePlan{EnvironmentId: "domains", Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	routers, ports, routes := map[string]*Router{}, map[string]*RouterPort{}, map[string]*Route{}
	for _, item := range models {
		switch v := item.(type) {
		case *Router:
			routers[v.Name] = v
		case *RouterPort:
			ports[v.Name] = v
		case *Route:
			routes[v.UUID] = v
		}
	}
	if len(routers) != 2 || len(ports) != 4 || len(routes) != 2 {
		t.Fatalf("wrong topology size: %d routers, %d ports, %d routes", len(routers), len(ports), len(routes))
	}
	for i, id := range []string{"r1", "r2"} {
		router := routers[objectName("lr", "domains", id)]
		if router == nil || len(router.Ports) != 2 || len(router.Routes) != 1 {
			t.Fatalf("router %s lost its own ports or table: %+v", id, router)
		}
		out := "p2"
		if i == 1 {
			out = "p4"
		}
		if *routes[router.Routes[0]].OutputPort != objectName("lrp", "domains", out) {
			t.Fatal("route points to a different router's interface")
		}
	}
	if ports[objectName("lrp", "domains", "p1")].Networks[0] != ports[objectName("lrp", "domains", "p3")].Networks[0] {
		t.Fatal("overlapping CIDRs were rewritten instead of isolated")
	}
}

func TestCompileLayerTwoWithoutImplicitRouter(t *testing.T) {
	routers := []api.LogicalRouter{}
	models, err := Compile(api.NodePlan{EnvironmentId: "l2", Spec: api.EnvironmentSpec{Routers: &routers, Networks: []api.Network{{Id: "lan", Name: "LAN", Cidr: "192.0.2.0/24"}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range models {
		if _, ok := item.(*Router); ok {
			t.Fatal("pure layer two topology gained an implicit router")
		}
	}
}

func TestManagementACLIsBoundToItsSwitchPort(t *testing.T) {
	address := "192.0.2.254"
	plan := api.NodePlan{EnvironmentId: "management", Spec: api.EnvironmentSpec{Routers: &[]api.LogicalRouter{}, Networks: []api.Network{{Id: "lan", Name: "LAN", Cidr: "192.0.2.0/24", AccessAddress: &address}}, Policies: &[]api.Policy{{Id: "deny", NetworkId: "lan", Direction: api.Both, Action: api.Deny}}}}
	models, err := Compile(plan)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, item := range models {
		if acl, ok := item.(*ACL); ok && acl.Priority == 32700 {
			count++
			if acl.Action != "allow-stateless" || !strings.Contains(acl.Match, accessNetworkPort("management", "lan")) || strings.Contains(acl.Match, address) {
				t.Fatal("management ACL trusts an address instead of the isolated platform port", acl)
			}
		}
	}
	if count != 2 {
		t.Fatal("management access did not cover both ACL stages")
	}
}
