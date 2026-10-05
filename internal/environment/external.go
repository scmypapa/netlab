package environment

import (
	"reflect"

	"netlab.local/core/api"
	"netlab.local/core/internal/access"
)

func AuthorizeHostBindings(identity access.Identity, before, after api.EnvironmentSpec) error {
	if identity.Administrator() {
		return nil
	}
	previous := map[string]api.Network{}
	for _, network := range before.Networks {
		previous[network.Id] = network
	}
	for _, network := range after.Networks {
		if network.External == nil {
			continue
		}
		old := previous[network.Id]
		if !reflect.DeepEqual(old.External, network.External) || old.Cidr != network.Cidr || !reflect.DeepEqual(old.AllocationPool, network.AllocationPool) {
			return access.ErrForbidden
		}
	}
	bindings := map[string]*api.PciBinding{}
	for _, asset := range before.Assets {
		bindings[asset.Id] = asset.PciBinding
	}
	for _, asset := range after.Assets {
		if asset.PciBinding != nil && !reflect.DeepEqual(bindings[asset.Id], asset.PciBinding) {
			return access.ErrForbidden
		}
	}
	return nil
}
