package environment

import (
	"errors"
	"testing"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
)

func TestServiceChangesRespectAssetPermissionsAndDependentRemoval(t *testing.T) {
	nic := api.Interface{Id: "nic", NetworkId: "lan", Primary: true}
	asset := api.Asset{Id: "web", Interfaces: []api.Interface{nic}}
	service := api.ServiceExposure{Id: "http", AssetId: "web", InterfaceId: "nic", Protocol: "tcp", TargetPort: 80}
	before := api.EnvironmentSpec{Assets: []api.Asset{asset}, Networks: []api.Network{}, Services: &[]api.ServiceExposure{service}}
	row := queries.Environment{ID: "env", ProjectID: "project"}
	identity := access.Identity{Principal: queries.Principal{ID: "token", Kind: "token"}, Grants: []queries.Grant{{ScopeKind: "asset", ScopeID: "env/web", Permissions: []string{"access"}}}}
	changed := service
	changed.TargetPort = 8080
	after := before
	after.Services = &[]api.ServiceExposure{changed}
	if err := AuthorizeChange(identity, row, before, after); err != nil {
		t.Fatal(err)
	}
	after.Assets = []api.Asset{{Id: "web", Name: "changed", Interfaces: asset.Interfaces}}
	if !errors.Is(AuthorizeChange(identity, row, before, after), access.ErrForbidden) {
		t.Fatal("asset access could change environment configuration")
	}
	identity.Grants = []queries.Grant{{ScopeKind: "environment", ScopeID: "env", Permissions: []string{"compose"}}}
	after = before
	after.Services = nil
	if !errors.Is(AuthorizeChange(identity, row, before, after), access.ErrForbidden) {
		t.Fatal("compose could explicitly revoke a live asset service")
	}
	for _, assets := range [][]api.Asset{{}, {{Id: "web", Interfaces: []api.Interface{}}}} {
		after = before
		after.Assets = assets
		after = RemoveDependentServices(before, after)
		if len(Services(after)) != 0 || AuthorizeChange(identity, row, before, after) != nil {
			t.Fatal("dependent service removal required extra access permission")
		}
	}
	after = before
	after.Services = &[]api.ServiceExposure{changed}
	if !errors.Is(AuthorizeChange(identity, row, before, after), access.ErrForbidden) {
		t.Fatal("compose could create or update a mapping")
	}
}

func TestServicesUseOwnedInterfacesAndBlueprintIdentities(t *testing.T) {
	port := 8080
	service := api.ServiceExposure{Id: "http", AssetId: "web", InterfaceId: "nic", Protocol: "tcp", TargetPort: 80, ListenPort: &port}
	spec := api.EnvironmentSpec{Assets: []api.Asset{{Id: "web", Interfaces: []api.Interface{{Id: "nic", NetworkId: "lan"}}}}, Networks: []api.Network{{Id: "lan"}}, Services: &[]api.ServiceExposure{service}}
	invalid := spec
	wrong := service
	wrong.InterfaceId = "another-asset-nic"
	invalid.Services = &[]api.ServiceExposure{wrong}
	if _, err := normalizeServices(invalid); err == nil {
		t.Fatal("service accepted a different asset interface")
	}
	if len(Services(RemoveDependentServices(spec, invalid))) != 1 {
		t.Fatal("invalid explicit service was silently removed")
	}
	view := api.CanvasView{}
	instantiate(&spec, &view)
	resolved := Services(spec)[0]
	if resolved.Id == service.Id || resolved.AssetId != spec.Assets[0].Id || resolved.InterfaceId != spec.Assets[0].Interfaces[0].Id || resolved.ListenPort != nil || resolved.TargetPort != 80 {
		t.Fatal("blueprint retained external ports or stale service references")
	}
}
