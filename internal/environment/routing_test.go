package environment

import (
	"encoding/json"
	"reflect"
	"testing"

	"netlab.local/core/api"
)

func routingSpec(t *testing.T, raw string) api.EnvironmentSpec {
	t.Helper()
	var spec api.EnvironmentSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestRoutingDomainsAndTransit(t *testing.T) {
	spec := routingSpec(t, `{"networks":[{"id":"left","name":"Left","cidr":"10.0.0.0/24"},{"id":"right","name":"Right","cidr":"10.0.0.0/24"},{"id":"transit","name":"Transit","cidr":"172.20.0.0/30"}],"routers":[{"id":"r1","name":"Left router","interfaces":[{"id":"r1lan","networkId":"left","address":"10.0.0.1"},{"id":"r1wan","networkId":"transit","address":"172.20.0.1"}]},{"id":"r2","name":"Right router","interfaces":[{"id":"r2lan","networkId":"right","address":"10.0.0.1"},{"id":"r2wan","networkId":"transit","address":"172.20.0.2"}]}]}`)
	result, err := Normalize(spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(*result.Routers) != 2 || result.Networks[2].AccessAddress != nil || *result.Networks[0].Gateway != "10.0.0.1" {
		t.Fatalf("routing domains or transit changed: %+v", result)
	}
	result.Routers = &[]api.LogicalRouter{{Id: "joined", Name: "Joined", Interfaces: []api.RouterInterface{{Id: "one", NetworkId: "left", Address: "10.0.0.1"}, {Id: "two", NetworkId: "right", Address: "10.0.0.1"}}}}
	if _, err = Normalize(result, nil); err == nil {
		t.Fatal("one router accepted overlapping directly connected prefixes")
	}
}

func TestLayerTwoAndStableManagementAddress(t *testing.T) {
	templates := map[string]api.Template{"container": {Kind: api.Container, Resources: api.Resources{Cpu: 0.25, MemoryMiB: 64, DiskGiB: 1}}}
	spec := routingSpec(t, `{"networks":[{"id":"lan","name":"LAN","cidr":"192.0.2.0/24"}],"routers":[],"assets":[{"id":"web","name":"Web","templateId":"container","interfaces":[{"id":"nic","networkId":"lan","address":"192.0.2.254"}]}]}`)
	result, err := Normalize(spec, templates)
	if err != nil {
		t.Fatal(err)
	}
	if result.Networks[0].Gateway != nil || *result.Networks[0].AccessAddress != "192.0.2.253" || len(*result.Routers) != 0 {
		t.Fatalf("layer two network acquired a business router or wrong management address: %+v", result)
	}
	again, err := Normalize(result, templates)
	if err != nil || !reflect.DeepEqual(result, again) {
		t.Fatalf("normalization is not stable: %v", err)
	}
	result.Assets[0].Interfaces[0].Address = *result.Networks[0].AccessAddress
	if _, err := Normalize(result, templates); err == nil {
		t.Fatal("asset reused management address")
	}
}

func TestRouterAutoAddressAvoidsExplicitAssets(t *testing.T) {
	spec := routingSpec(t, `{"networks":[{"id":"lan","name":"LAN","cidr":"192.0.2.0/24"}],"routers":[{"id":"r","name":"Router","interfaces":[{"id":"port","networkId":"lan","address":""}]}],"assets":[{"id":"web","name":"Web","templateId":"container","interfaces":[{"id":"nic","networkId":"lan","address":"192.0.2.1"}]}]}`)
	result, err := Normalize(spec, map[string]api.Template{"container": {Resources: api.Resources{Cpu: 1, MemoryMiB: 64, DiskGiB: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if (*result.Routers)[0].Interfaces[0].Address != "192.0.2.2" || *result.Networks[0].Gateway != "192.0.2.2" {
		t.Fatal("automatic router address collided with explicitly assigned asset")
	}
}

func TestStaticRouteOwnerAndNextHop(t *testing.T) {
	base := `{"networks":[{"id":"transit","name":"Transit","cidr":"172.20.0.0/29"}],"routers":[{"id":"r1","name":"R1","interfaces":[{"id":"p1","networkId":"transit","address":"172.20.0.1"}]},{"id":"r2","name":"R2","interfaces":[{"id":"p2","networkId":"transit","address":"172.20.0.2"}]}],"routes":[{"routerId":"r1","networkId":"transit","destination":"10.1.0.99/24","nextHop":"172.20.0.2"}]}`
	result, err := Normalize(routingSpec(t, base), nil)
	if err != nil || (*result.Routes)[0].Destination != "10.1.0.0/24" {
		t.Fatalf("valid route failed: %v", err)
	}
	for _, test := range []struct {
		name string
		edit func(*api.EnvironmentSpec)
	}{
		{"ambiguous owner", func(s *api.EnvironmentSpec) { (*s.Routes)[0].RouterId = nil }},
		{"absent router", func(s *api.EnvironmentSpec) { id := "missing"; (*s.Routes)[0].RouterId = &id }},
		{"self next hop", func(s *api.EnvironmentSpec) { (*s.Routes)[0].NextHop = "172.20.0.1" }},
		{"off-link next hop", func(s *api.EnvironmentSpec) { (*s.Routes)[0].NextHop = "172.21.0.1" }},
		{"address family", func(s *api.EnvironmentSpec) { (*s.Routes)[0].Destination = "fd00::/64" }},
		{"duplicate destination", func(s *api.EnvironmentSpec) { *s.Routes = append(*s.Routes, (*s.Routes)[0]) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := routingSpec(t, base)
			test.edit(&spec)
			if _, err := Normalize(spec, nil); err == nil {
				t.Fatal("invalid route accepted")
			}
		})
	}
}

func TestRouterIdentityAndBlueprintRemapping(t *testing.T) {
	spec := routingSpec(t, `{"networks":[{"id":"lan","name":"LAN","cidr":"10.1.0.0/24"}],"routers":[{"id":"r1","name":"R1","interfaces":[{"id":"p1","networkId":"lan","address":"10.1.0.1"}]}],"routes":[{"routerId":"r1","networkId":"lan","destination":"10.2.0.0/24","nextHop":"10.1.0.2"}]}`)
	positions := map[string]api.Point{"r1": {X: 10, Y: 20}}
	view := api.CanvasView{Positions: &positions}
	instantiate(&spec, &view)
	router := (*spec.Routers)[0]
	if router.Id == "r1" || router.Interfaces[0].Id == "p1" || router.Interfaces[0].NetworkId != spec.Networks[0].Id || *(*spec.Routes)[0].RouterId != router.Id || (*view.Positions)[router.Id].X != 10 {
		t.Fatal("cloned router identities or references were not remapped")
	}
	if _, err := Normalize(spec, nil); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultRouterDoesNotReuseBusinessIdentity(t *testing.T) {
	spec := routingSpec(t, `{"networks":[{"id":"gateway","name":"LAN","cidr":"192.0.2.0/24"}],"assets":[{"id":"gateway_","templateId":"c","interfaces":[{"id":"gateway","networkId":"gateway"}]}]}`)
	result, err := Normalize(spec, map[string]api.Template{"c": {Resources: api.Resources{Cpu: 1, MemoryMiB: 64, DiskGiB: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if (*result.Routers)[0].Id != "gateway__" {
		t.Fatal("default router reused a business object's identity")
	}
	if (*result.Routers)[0].Interfaces[0].Id == result.Assets[0].Interfaces[0].Id {
		t.Fatal("default router reused an existing guest interface identity")
	}
	empty := (api.EnvironmentSpec{}).WithRouting()
	if len(*empty.Routers) != 0 {
		t.Fatal("empty environment gained a phantom router")
	}
}

func TestNetworkEditsPreserveServerOwnedAddress(t *testing.T) {
	address := "192.0.2.253"
	before := api.EnvironmentSpec{Networks: []api.Network{{Id: "lan", Name: "LAN", Cidr: "192.0.2.0/24", AccessAddress: &address}}}
	after := api.EnvironmentSpec{Networks: []api.Network{{Id: "lan", Name: "Renamed", Cidr: "192.0.2.0/24"}}}
	after = preserveNetworkAccess(before, after)
	if after.Networks[0].AccessAddress == nil || *after.Networks[0].AccessAddress != address {
		t.Fatal("omitting a read-only field reallocated the management endpoint")
	}
	after.Networks[0].Cidr = "192.0.3.0/24"
	after = preserveNetworkAccess(before, after)
	if after.Networks[0].AccessAddress != nil {
		t.Fatal("new address pool kept an address outside its subnet")
	}
}
