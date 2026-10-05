package environment

import (
	"errors"
	"netlab.local/core/api"
	"netlab.local/core/internal/access"
	"testing"
)

func TestHostBindingAuthorization(t *testing.T) {
	binding := &api.PciBinding{NodeId: "node", GroupIds: []string{"group"}}
	spec := api.EnvironmentSpec{Assets: []api.Asset{{Id: "asset", PciBinding: binding}}}
	if err := AuthorizeHostBindings(access.Identity{}, api.EnvironmentSpec{}, spec); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("non-admin bound host hardware", err)
	}
	if err := AuthorizeHostBindings(access.Identity{}, spec, spec); err != nil {
		t.Fatal("ordinary edit changed authorization", err)
	}
	if err := AuthorizeHostBindings(access.Identity{}, spec, api.EnvironmentSpec{}); err != nil {
		t.Fatal("removal blocked", err)
	}
}

func TestExternalAddressOwnership(t *testing.T) {
	pool, gateway := "192.0.2.128/26", "192.0.2.1"
	spec := api.EnvironmentSpec{Networks: []api.Network{{Id: "lan", Name: "LAN", Cidr: "192.0.2.0/24", Gateway: &gateway, AllocationPool: &pool, External: &api.ExternalAttachment{NodeId: "node", Interface: "eth1"}}}, Assets: []api.Asset{{Id: "web", TemplateId: "web", Interfaces: []api.Interface{{Id: "nic", NetworkId: "lan"}}}}}
	normalized, err := Normalize(spec, map[string]api.Template{"web": {Resources: api.Resources{Cpu: 1, MemoryMiB: 64, DiskGiB: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	network := normalized.Networks[0]
	if *network.Gateway != gateway || network.RouterAddress() != "192.0.2.129" || normalized.Assets[0].Interfaces[0].Address != "192.0.2.130" {
		t.Fatalf("LAN gateway or product allocation changed: %+v", normalized)
	}
	resolved := Resolve(normalized, normalized.Assets[0], nil)[0]
	if resolved.Prefix != 24 || *resolved.Gateway != gateway {
		t.Fatalf("guest uses allocation prefix or product router: %+v", resolved)
	}
	normalized.Assets[0].Interfaces[0].Address = "192.0.2.10"
	if _, err := Normalize(normalized, map[string]api.Template{"web": {Resources: api.Resources{Cpu: 1, MemoryMiB: 64, DiskGiB: 1}}}); err == nil {
		t.Fatal("address outside product allocation accepted")
	}
}

func TestExternalLANWithoutGateway(t *testing.T) {
	pool := "fd12::100/120"
	spec, err := Normalize(api.EnvironmentSpec{Networks: []api.Network{{Id: "lan", Name: "IPv6", Cidr: "fd12::/64", AllocationPool: &pool, External: &api.ExternalAttachment{NodeId: "node", Interface: "eth1"}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Networks[0].Gateway != nil || spec.Networks[0].RouterAddress() != "fd12::101" {
		t.Fatalf("invented external gateway: %+v", spec)
	}
	gateway := "fe80::1"
	spec.Networks[0].Gateway = &gateway
	if _, err := Normalize(spec, nil); err != nil {
		t.Fatal(err)
	}
}
