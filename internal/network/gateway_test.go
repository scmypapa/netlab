package network

import (
	"net/netip"
	"testing"

	"netlab.local/core/api"
)

func TestServiceGatewayNativeModels(t *testing.T) {
	prefix := netip.MustParsePrefix("100.127.0.0/16")
	port := 24443
	bindings := []api.NodeServiceBinding{{Id: "web", AssetId: "vm", InterfaceId: "lan", Protocol: api.Tcp, TargetAddress: "192.168.10.2", TargetPort: 443, ListenPort: port}, {Id: "dns", AssetId: "container", InterfaceId: "lan", Protocol: api.Udp, TargetAddress: "192.168.10.3", TargetPort: 53, ListenPort: 0}}
	plan := api.NodePlan{EnvironmentId: "environment", Gateway: &api.ServiceGateway{NodeId: "node-business-id", Address: "100.127.0.2"}, Services: &bindings, Spec: api.EnvironmentSpec{Networks: []api.Network{{Id: "lan", Name: "LAN", Cidr: "192.168.10.0/24"}}}}
	router := &Router{}
	models, err := gatewayModels(plan, router, prefix, "real-ovs-system-id")
	if err != nil {
		t.Fatal(err)
	}
	var lb *LoadBalancer
	for _, model := range models {
		switch current := model.(type) {
		case *GatewayChassis:
			if current.ChassisName != "real-ovs-system-id" {
				t.Fatal("gateway used the business node ID as an OVN chassis")
			}
		case *LoadBalancer:
			if lb != nil {
				t.Fatal("unallocated auto port was compiled as an OVN VIP")
			}
			lb = current
		}
	}
	if lb == nil || lb.VIPs["100.127.0.2:24443"] != "192.168.10.2:443" || router.Options["lb_force_snat_ip"] != "router_ip" {
		t.Fatal("native mapping or return SNAT was not configured")
	}
	if len(router.LoadBalancers) != 1 || len(router.Ports) != 1 {
		t.Fatal("gateway did not attach to the environment router")
	}
	first := lb.Name
	plan.Assets = []api.AssetExecution{{InstanceId: "replacement-instance"}}
	models, err = gatewayModels(plan, &Router{}, prefix, "real-ovs-system-id")
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range models {
		if current, ok := model.(*LoadBalancer); ok && current.Name != first {
			t.Fatal("replacement changed the stable service identity")
		}
	}
	plan.Spec.Networks[0].Cidr = "100.127.0.0/24"
	if _, err = gatewayModels(plan, &Router{}, prefix, "real-ovs-system-id"); err == nil {
		t.Fatal("overlapping provider and guest CIDRs were accepted")
	}
}
