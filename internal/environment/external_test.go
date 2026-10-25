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

func TestExternalDevicesUseExistingAddressAndReserveIt(t *testing.T) {
	pool := "192.0.2.128/26"
	spec := api.EnvironmentSpec{
		Networks: []api.Network{{Id: "lan", Name: "现场", Cidr: "192.0.2.0/24", External: &api.ExternalAttachment{NodeId: "node", Interface: "eth1"}, AllocationPool: &pool,
			Devices: []api.ExternalDevice{{Id: "plc", Name: "PLC", Address: "192.0.2.130", Mac: "02:00:00:00:01:20"}}}},
		Assets: []api.Asset{{Id: "vm", TemplateId: "test", Interfaces: []api.Interface{{Id: "nic", NetworkId: "lan"}}}},
	}
	templates := map[string]api.Template{"test": {Resources: api.Resources{Cpu: 1, MemoryMiB: 512, DiskGiB: 5}}}
	normalized, err := Normalize(spec, templates)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Networks[0].Devices[0].Address != "192.0.2.130" || normalized.Assets[0].Interfaces[0].Address != "192.0.2.131" {
		t.Fatal("physical address changed or allocated twice", normalized)
	}
	after := normalized
	after.Networks = append([]api.Network{}, normalized.Networks...)
	after.Networks[0].Devices = nil
	preview := Diff(1, normalized, after, templates)
	if len(preview.Changes) != 1 || preview.Changes[0].Kind != api.ChangeItemKindNetwork {
		t.Fatal("removing registration restarted a virtual asset", preview)
	}
	normalized.Assets[0].Interfaces[0].Address = "192.0.2.130"
	if _, err := Normalize(normalized, templates); err == nil {
		t.Fatal("physical address reused by virtual asset")
	}
}
