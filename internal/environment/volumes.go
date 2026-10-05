package environment

import (
	"context"
	"slices"
	"strings"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
)

func PersistentVolumeIDs(assets []api.Asset) []string {
	ids := []string{}
	for _, asset := range assets {
		if asset.Volumes != nil {
			for _, volume := range *asset.Volumes {
				if volume.PersistentVolumeId != nil {
					ids = append(ids, *volume.PersistentVolumeId)
				}
			}
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

func AuthorizeVolumes(identity access.Identity, before, after []api.Asset) error {
	old := PersistentVolumeIDs(before)
	for _, id := range PersistentVolumeIDs(after) {
		if !slices.Contains(old, id) && !identity.Administrator() {
			return access.ErrForbidden
		}
	}
	return nil
}

func ResolveVolumes(ctx context.Context, q *queries.Queries, spec api.EnvironmentSpec) (api.EnvironmentSpec, error) {
	ids := PersistentVolumeIDs(spec.Assets)
	if len(ids) == 0 {
		return spec, nil
	}
	rows, err := q.GetPersistentVolumes(ctx, ids)
	if err != nil {
		return spec, err
	}
	if len(rows) != len(ids) {
		return spec, Invalid("数据卷不存在")
	}
	volumes := map[string]queries.PersistentVolume{}
	for _, row := range rows {
		volumes[row.ID] = row
	}
	templates, err := Templates(ctx, q, spec.Assets)
	if err != nil {
		return spec, err
	}
	seen := map[string]bool{}
	for i := range spec.Assets {
		a := &spec.Assets[i]
		poolID := ""
		if a.StoragePoolId != nil {
			poolID = *a.StoragePoolId
		}
		if a.Volumes == nil {
			continue
		}
		for j := range *a.Volumes {
			volume := &(*a.Volumes)[j]
			if volume.PersistentVolumeId == nil {
				continue
			}
			id := *volume.PersistentVolumeId
			if seen[id] {
				return spec, Invalid("数据卷只能附加到一个资产")
			}
			seen[id] = true
			row := volumes[id]
			if row.State != "ready" {
				return spec, Invalid("数据卷 %s 尚不可用", row.Name)
			}
			if row.Kind != string(templates[a.TemplateId].Kind) {
				return spec, Invalid("数据卷 %s 的类型与资产不一致", row.Name)
			}
			volume.SizeGiB = row.SizeGib
			if poolID != "" && poolID != row.StoragePoolID {
				return spec, Invalid("同一资产的磁盘应位于同一存储池")
			}
			poolID = row.StoragePoolID
			// The volume's storage decides placement; no extra node selection is needed.
			if strings.HasPrefix(row.StoragePoolID, "default:") {
				a.StoragePoolId = nil
			} else {
				a.StoragePoolId = &row.StoragePoolID
			}
		}
	}
	return spec, nil
}

func CheckVolumeUses(ctx context.Context, q *queries.Queries, assets []api.Asset, environmentID string) error {
	owners := map[string]string{}
	for _, asset := range assets {
		if asset.Volumes == nil {
			continue
		}
		for _, volume := range *asset.Volumes {
			if volume.PersistentVolumeId == nil {
				continue
			}
			id := *volume.PersistentVolumeId
			if owner, exists := owners[id]; exists && owner != asset.Id {
				return ErrInUse
			}
			owners[id] = asset.Id
		}
	}
	uses, err := q.PersistentVolumeUses(ctx, PersistentVolumeIDs(assets))
	if err != nil {
		return err
	}
	for _, use := range uses {
		if use.EnvironmentID != environmentID || use.AssetID != owners[use.VolumeID] {
			return ErrInUse
		}
	}
	return nil
}
