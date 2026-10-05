package environment

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
)

type creationSource struct {
	Spec       api.EnvironmentSpec
	View       api.CanvasView
	Recovery   *queries.RecoveryPoint
	Backup     *queries.Backup
	Definition json.RawMessage
}

func (s Service) CreationSpec(ctx context.Context, identity access.Identity, request api.CreateEnvironment) (creationSource, error) {
	var source creationSource
	count := 0
	for _, present := range []bool{request.Spec != nil, request.BlueprintVersionId != nil, request.RecoveryPointId != nil, request.BackupId != nil} {
		if present {
			count++
		}
	}
	if count != 1 {
		return source, Invalid("请选择一个环境模板、恢复点、备份或环境设计")
	}
	if request.Spec != nil {
		source.Spec = *request.Spec
		return source, nil
	}
	if request.BackupId != nil {
		backup, err := s.Queries.GetBackup(ctx, *request.BackupId)
		if err != nil {
			return source, err
		}
		if backup.EnvironmentID != nil {
			if _, err = s.Authorized(ctx, identity, *backup.EnvironmentID, "manage", ""); err != nil {
				return source, err
			}
		} else if !identity.Administrator() {
			return source, access.ErrForbidden
		}
		if backup.State != "ready" {
			return source, Invalid("备份尚不可用")
		}
		var definition struct {
			Recovery json.RawMessage `json:"recovery"`
			View     api.CanvasView  `json:"view"`
		}
		if err = json.Unmarshal(backup.Definition, &definition); err != nil {
			return source, err
		}
		var captured struct {
			Spec api.EnvironmentSpec `json:"spec"`
		}
		if err = json.Unmarshal(definition.Recovery, &captured); err != nil {
			return source, err
		}
		source.Spec, source.Definition, source.View, source.Backup = captured.Spec, definition.Recovery, definition.View, &backup
		clearRecoveryPlacement(&source.Spec)
		return source, nil
	}
	if request.RecoveryPointId != nil {
		point, err := s.Queries.GetRecoveryPoint(ctx, *request.RecoveryPointId)
		if err != nil {
			return source, err
		}
		env, err := s.Authorized(ctx, identity, point.EnvironmentID, "manage", "")
		if err != nil {
			return source, err
		}
		if point.State != "ready" {
			return source, Invalid("恢复点尚不可用")
		}
		var captured struct {
			Spec api.EnvironmentSpec `json:"spec"`
		}
		if err = json.Unmarshal(point.Definition, &captured); err != nil {
			return source, err
		}
		source.Spec, source.Recovery, source.Definition = captured.Spec, &point, point.Definition
		clearRecoveryPlacement(&source.Spec)
		err = json.Unmarshal(env.View, &source.View)
		return source, err
	}
	row, err := s.Queries.GetBlueprintVersion(ctx, *request.BlueprintVersionId)
	if err != nil {
		return source, err
	}
	if !identity.Allows("read", row.ProjectID, "", "", row.OwnerID) {
		return source, access.ErrForbidden
	}
	if err = json.Unmarshal(row.Spec, &source.Spec); err != nil {
		return source, err
	}
	if err = json.Unmarshal(row.View, &source.View); err != nil {
		return source, err
	}
	instantiate(&source.Spec, &source.View)
	return source, nil
}

func clearRecoveryPlacement(spec *api.EnvironmentSpec) {
	for i := range spec.Assets {
		spec.Assets[i].StoragePoolId = nil
		if spec.Assets[i].Volumes != nil {
			copies := append([]api.Volume{}, (*spec.Assets[i].Volumes)...)
			for j := range copies {
				copies[j].PersistentVolumeId = nil
			}
			spec.Assets[i].Volumes = &copies
		}
	}
	if spec.Services != nil {
		for i := range *spec.Services {
			(*spec.Services)[i].ListenPort = nil
		}
	}
}

// Each creation has its own logical identities; addresses remain in isolated networks.
func instantiate(spec *api.EnvironmentSpec, view *api.CanvasView) {
	networks, assets, interfaces, objects := map[string]string{}, map[string]string{}, map[string]string{}, map[string]string{}
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
			interfaces[iface.Id] = uuid.NewString()
			iface.Id, iface.NetworkId = interfaces[iface.Id], networks[iface.NetworkId]
		}
		if asset.Volumes != nil {
			for j := range *asset.Volumes {
				(*asset.Volumes)[j].Id = uuid.NewString()
				(*asset.Volumes)[j].PersistentVolumeId = nil
			}
		}
	}
	if spec.Services != nil {
		for i := range *spec.Services {
			service := &(*spec.Services)[i]
			service.Id, service.AssetId, service.InterfaceId = uuid.NewString(), assets[service.AssetId], interfaces[service.InterfaceId]
			service.ListenPort = nil
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
