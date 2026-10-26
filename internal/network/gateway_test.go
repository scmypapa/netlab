package network

import (
	"net/netip"
	"netlab.local/core/api"
	"testing"
)

func TestServiceGatewayNativeModels(t *testing.T) {
	address := "10.0.0.254"
	record := &accessRecord{Version: 2, Networks: []api.Network{{Id: "left", Cidr: "10.0.0.0/24", AccessAddress: &address}, {Id: "right", Cidr: "10.0.0.0/24", AccessAddress: &address}}}
	gateway := &api.ServiceGateway{NodeId: "node", Address: "100.127.0.2"}
	models, err := accessModels("environment", record, gateway, netip.MustParsePrefix("100.127.0.0/16"))
	if err != nil {
		t.Fatal(err)
	}
	ports := map[string]bool{}
	for _, item := range models {
		switch value := item.(type) {
		case *SwitchPort:
			if id := value.ExternalIDs["netlab.network"]; id != "" {
				ports[id] = true
			}
		case *Router, *RouterPort, *LoadBalancer, *NAT:
			t.Fatal("management access changes business routing", item)
		}
	}
	if !ports["left"] || !ports["right"] {
		t.Fatal("overlapping networks lost their own management ports")
	}
	record.Networks[0].Cidr = "100.127.0.0/24"
	if _, err = accessModels("environment", record, gateway, netip.MustParsePrefix("100.127.0.0/16")); err == nil {
		t.Fatal("provider overlap accepted")
	}
}
