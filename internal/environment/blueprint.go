package environment

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"netlab.local/core/api"
	"netlab.local/core/internal/access"
)

func (s Service) CreationSpec(ctx context.Context, identity access.Identity, request api.CreateEnvironment) (api.EnvironmentSpec, api.CanvasView, error) {
	if (request.Spec == nil) == (request.BlueprintVersionId == nil) {
		return api.EnvironmentSpec{}, api.CanvasView{}, Invalid("请选择环境模板版本或提供环境设计")
	}
	if request.Spec != nil {
		return *request.Spec, api.CanvasView{}, nil
	}
	row, err := s.Queries.GetBlueprintVersion(ctx, *request.BlueprintVersionId)
	if err != nil {
		return api.EnvironmentSpec{}, api.CanvasView{}, err
	}
	if !identity.Allows("read", row.ProjectID, "", "", row.OwnerID) {
		return api.EnvironmentSpec{}, api.CanvasView{}, access.ErrForbidden
	}
	var spec api.EnvironmentSpec
	var view api.CanvasView
	if err = json.Unmarshal(row.Spec, &spec); err != nil {
		return spec, view, err
	}
	if err = json.Unmarshal(row.View, &view); err != nil {
		return spec, view, err
	}
	instantiate(&spec, &view)
	return spec, view, nil
}

// Each creation has its own logical identities; addresses remain in isolated networks.
func instantiate(spec *api.EnvironmentSpec, view *api.CanvasView) {
	networks, assets, objects := map[string]string{}, map[string]string{}, map[string]string{}
	for i := range spec.Networks {
		network := &spec.Networks[i]
		id := uuid.NewString()
		networks[network.Id], objects[network.Id] = id, id
		network.Id = id
	}
	for i := range spec.Assets {
		asset := &spec.Assets[i]
		id := uuid.NewString()
		assets[asset.Id], objects[asset.Id] = id, id
		asset.Id = id
		for j := range asset.Interfaces {
			iface := &asset.Interfaces[j]
			iface.Id, iface.NetworkId = uuid.NewString(), networks[iface.NetworkId]
		}
		if asset.Volumes != nil {
			for j := range *asset.Volumes {
				(*asset.Volumes)[j].Id = uuid.NewString()
			}
		}
	}
	for i := range spec.Networks {
		network := &spec.Networks[i]
		if network.DnsAssetId != nil {
			id := assets[*network.DnsAssetId]
			network.DnsAssetId = &id
		}
	}
	if spec.Routes != nil {
		for i := range *spec.Routes {
			(*spec.Routes)[i].NetworkId = networks[(*spec.Routes)[i].NetworkId]
		}
	}
	if spec.Policies != nil {
		for i := range *spec.Policies {
			policy := &(*spec.Policies)[i]
			policy.Id, policy.NetworkId = uuid.NewString(), networks[policy.NetworkId]
		}
	}
	if view.Positions != nil {
		positions := make(map[string]api.Point, len(*view.Positions))
		for id, position := range *view.Positions {
			if mapped, ok := objects[id]; ok {
				positions[mapped] = position
			}
		}
		view.Positions = &positions
	}
	if view.Collapsed != nil {
		collapsed := make([]string, 0, len(*view.Collapsed))
		for _, id := range *view.Collapsed {
			if mapped, ok := objects[id]; ok {
				collapsed = append(collapsed, mapped)
			}
		}
		view.Collapsed = &collapsed
	}
}
