package environment

import (
	"context"
	"encoding/json"
	"slices"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
)

// A nil asset set means the caller can read the entire environment.
func (s Service) Readable(ctx context.Context, identity access.Identity, id string) (queries.Environment, map[string]bool, error) {
	row, err := s.Queries.GetEnvironment(ctx, id)
	if err != nil || identity.Allows("read", row.ProjectID, row.ID, "", row.OwnerID) {
		return row, nil, err
	}
	raw := row.AppliedSpec
	if raw == nil {
		raw = row.Spec
	}
	var spec api.EnvironmentSpec
	if err = json.Unmarshal(raw, &spec); err != nil {
		return row, nil, err
	}
	visible := map[string]bool{}
	for _, asset := range spec.Assets {
		if identity.Allows("read", row.ProjectID, row.ID, asset.Id, row.OwnerID) {
			visible[asset.Id] = true
		}
	}
	if len(visible) == 0 {
		return row, nil, access.ErrForbidden
	}
	return row, visible, nil
}

func VisibleRecord(row queries.Environment, visible map[string]bool) (api.Environment, error) {
	result, err := Record(row)
	if err != nil || visible == nil {
		return result, err
	}
	filter := func(spec api.EnvironmentSpec) api.EnvironmentSpec {
		networks := map[string]bool{}
		spec.Assets = slices.DeleteFunc(spec.Assets, func(asset api.Asset) bool { return !visible[asset.Id] })
		for _, asset := range spec.Assets {
			for _, iface := range asset.Interfaces {
				networks[iface.NetworkId] = true
			}
		}
		spec.Networks = slices.DeleteFunc(spec.Networks, func(network api.Network) bool { return !networks[network.Id] })
		for i := range spec.Networks {
			if dns := spec.Networks[i].DnsAssetId; dns != nil && !visible[*dns] {
				spec.Networks[i].DnsAssetId = nil
			}
		}
		spec.Routes, spec.Policies = nil, nil
		if spec.Services != nil {
			services := slices.DeleteFunc(slices.Clone(*spec.Services), func(service api.ServiceExposure) bool { return !visible[service.AssetId] })
			spec.Services = &services
		}
		return spec
	}
	result.Spec = filter(result.Spec)
	spec := result.Spec
	if result.AppliedSpec != nil {
		filtered := filter(*result.AppliedSpec)
		result.AppliedSpec = &filtered
		spec = filtered
	}
	positions := map[string]api.Point{}
	if result.View.Positions != nil {
		for _, asset := range spec.Assets {
			if point, exists := (*result.View.Positions)[asset.Id]; exists {
				positions[asset.Id] = point
			}
		}
		for _, network := range spec.Networks {
			if point, exists := (*result.View.Positions)[network.Id]; exists {
				positions[network.Id] = point
			}
		}
	}
	roles := map[string]string{}
	if result.View.Roles != nil {
		for _, asset := range spec.Assets {
			if role, exists := (*result.View.Roles)[asset.Id]; exists {
				roles[asset.Id] = role
			}
		}
	}
	result.View = api.CanvasView{Positions: &positions, Roles: &roles}
	result.Draft, result.OperationId, result.Error, result.BlueprintVersionId, result.ExternalReference = nil, nil, nil, nil, nil
	return result, nil
}
