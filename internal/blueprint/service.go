package blueprint

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/environment"
)

func (s Service) Delete(ctx context.Context, identity access.Identity, id string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	row, err := q.LockBlueprint(ctx, id)
	if err != nil {
		return err
	}
	if !identity.Allows("compose", row.ProjectID, "", "", row.OwnerID) {
		return access.ErrForbidden
	}
	if _, err = q.LockBlueprintVersions(ctx, id); err != nil {
		return err
	}
	references, err := q.BlueprintReferences(ctx, id)
	if err != nil {
		return err
	}
	if len(references) > 0 {
		return fmt.Errorf("%w：%s", environment.ErrInUse, strings.Join(references, "、"))
	}
	if err = q.DetachDestroyedBlueprint(ctx, id); err != nil {
		return err
	}
	if err = q.DeleteBlueprintVersions(ctx, id); err != nil {
		return err
	}
	if err = q.DeleteBlueprint(ctx, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type Service struct {
	Pool    *pgxpool.Pool
	Queries *queries.Queries
}

func (s Service) Authorized(ctx context.Context, identity access.Identity, id, permission string) (queries.GetBlueprintRow, error) {
	row, err := s.Queries.GetBlueprint(ctx, id)
	if err != nil {
		return row, err
	}
	if !identity.Allows(permission, row.ProjectID, "", "", row.OwnerID) {
		return row, access.ErrForbidden
	}
	return row, nil
}

func Record(row queries.GetBlueprintRow, identity access.Identity) api.Blueprint {
	permissions := identity.Permissions(row.ProjectID, "", "", row.OwnerID)
	return api.Blueprint{Id: row.ID, ProjectId: row.ProjectID, Name: row.Name, LatestVersionId: row.LatestVersionID, LatestVersion: int(row.Version), AssetCount: int(row.AssetCount), NetworkCount: int(row.NetworkCount), CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time, Permissions: &permissions}
}

func Version(row queries.BlueprintVersion) (api.BlueprintVersion, error) {
	result := api.BlueprintVersion{Id: row.ID, BlueprintId: row.BlueprintID, Version: int(row.Version), AssetCount: int(row.AssetCount), NetworkCount: int(row.NetworkCount), SourceEnvironmentId: row.SourceEnvironmentID, SourceRevision: int(row.SourceRevision), CreatedAt: row.CreatedAt.Time}
	if err := json.Unmarshal(row.Spec, &result.Spec); err != nil {
		return result, err
	}
	if err := json.Unmarshal(row.View, &result.View); err != nil {
		return result, err
	}
	return result, nil
}

func (s Service) GetVersion(ctx context.Context, identity access.Identity, id string) (api.BlueprintVersion, error) {
	row, err := s.Queries.GetBlueprintVersion(ctx, id)
	if err != nil {
		return api.BlueprintVersion{}, err
	}
	if !identity.Allows("read", row.ProjectID, "", "", row.OwnerID) {
		return api.BlueprintVersion{}, access.ErrForbidden
	}
	return Version(queries.BlueprintVersion{ID: row.ID, BlueprintID: row.BlueprintID, Version: row.Version, Spec: row.Spec, View: row.View, AssetCount: row.AssetCount, NetworkCount: row.NetworkCount, SourceEnvironmentID: row.SourceEnvironmentID, SourceRevision: row.SourceRevision, CreatedAt: row.CreatedAt})
}

func (s Service) Save(ctx context.Context, identity access.Identity, environmentID, blueprintID, name string, expectedRevision int, input api.EnvironmentSpec) (api.Blueprint, api.BlueprintVersion, error) {
	source, err := (environment.Service{Pool: s.Pool, Queries: s.Queries}).Authorized(ctx, identity, environmentID, "compose", "")
	if err != nil {
		return api.Blueprint{}, api.BlueprintVersion{}, err
	}
	if blueprintID == "" {
		name = strings.TrimSpace(name)
		if name == "" {
			return api.Blueprint{}, api.BlueprintVersion{}, environment.Invalid("请输入模板名称")
		}
		if !identity.Allows("compose", source.ProjectID, "", "", nil) {
			return api.Blueprint{}, api.BlueprintVersion{}, access.ErrForbidden
		}
	} else {
		_, err = s.Authorized(ctx, identity, blueprintID, "compose")
		if err != nil {
			return api.Blueprint{}, api.BlueprintVersion{}, err
		}
	}
	templates, err := environment.Templates(ctx, s.Queries, input.Assets)
	if err != nil {
		return api.Blueprint{}, api.BlueprintVersion{}, err
	}
	spec, err := environment.Normalize(input, templates)
	if err != nil {
		return api.Blueprint{}, api.BlueprintVersion{}, err
	}
	// Templates describe new disks, not attachments to existing environment data.
	for _, asset := range spec.Assets {
		if asset.Volumes != nil {
			for i := range *asset.Volumes {
				(*asset.Volumes)[i].PersistentVolumeId = nil
			}
		}
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return api.Blueprint{}, api.BlueprintVersion{}, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return api.Blueprint{}, api.BlueprintVersion{}, err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	source, err = q.LockEnvironment(ctx, environmentID)
	if err != nil {
		return api.Blueprint{}, api.BlueprintVersion{}, err
	}
	if int(source.Revision) != expectedRevision {
		return api.Blueprint{}, api.BlueprintVersion{}, environment.ErrConflict
	}
	if err = environment.ReferenceResources(ctx, q, spec.Assets); err != nil {
		return api.Blueprint{}, api.BlueprintVersion{}, err
	}
	if blueprintID == "" {
		blueprintID = uuid.NewString()
		var owner *string
		if identity.Principal.Kind == "user" {
			owner = &identity.Principal.ID
		}
		err = q.CreateBlueprint(ctx, queries.CreateBlueprintParams{ID: blueprintID, ProjectID: source.ProjectID, OwnerID: owner, Name: name})
		if err != nil {
			return api.Blueprint{}, api.BlueprintVersion{}, err
		}
	}
	allocated, err := q.NextBlueprintVersion(ctx, blueprintID)
	if err != nil {
		return api.Blueprint{}, api.BlueprintVersion{}, err
	}
	saved, err := q.CreateBlueprintVersion(ctx, queries.CreateBlueprintVersionParams{ID: uuid.NewString(), BlueprintID: blueprintID, Version: allocated.Version, Spec: raw, View: source.View, AssetCount: int32(len(spec.Assets)), NetworkCount: int32(len(spec.Networks)), SourceEnvironmentID: source.ID, SourceRevision: source.Revision})
	if err != nil {
		return api.Blueprint{}, api.BlueprintVersion{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return api.Blueprint{}, api.BlueprintVersion{}, err
	}
	result := api.Blueprint{Id: blueprintID, Name: allocated.Name, ProjectId: allocated.ProjectID, LatestVersion: int(saved.Version), LatestVersionId: saved.ID, AssetCount: int(saved.AssetCount), NetworkCount: int(saved.NetworkCount), CreatedAt: allocated.CreatedAt.Time, UpdatedAt: allocated.UpdatedAt.Time}
	permissions := identity.Permissions(allocated.ProjectID, "", "", allocated.OwnerID)
	result.Permissions = &permissions
	version, err := Version(saved)
	return result, version, err
}
