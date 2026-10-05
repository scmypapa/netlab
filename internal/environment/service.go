package environment

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
)

type Service struct {
	Pool    *pgxpool.Pool
	Queries *queries.Queries
}

var ErrConflict = errors.New("环境配置已变化，请重新确认本轮变更")
var ErrInUse = errors.New("资源仍在使用")

func Templates(ctx context.Context, q *queries.Queries, assets []api.Asset) (map[string]api.Template, error) {
	ids := make([]string, 0, len(assets))
	seen := map[string]bool{}
	for _, a := range assets {
		if !seen[a.TemplateId] {
			ids = append(ids, a.TemplateId)
			seen[a.TemplateId] = true
		}
	}
	rows, err := q.GetTemplates(ctx, ids)
	if err != nil {
		return nil, err
	}
	result := map[string]api.Template{}
	for _, row := range rows {
		var t api.Template
		if err = json.Unmarshal(row.Definition, &t); err != nil {
			return nil, err
		}
		result[t.Id] = t
	}
	return result, nil
}

// ReferenceResources holds resource rows until the referencing write commits.
func ReferenceResources(ctx context.Context, q *queries.Queries, assets []api.Asset) error {
	volumeIDs := PersistentVolumeIDs(assets)
	volumes, err := q.LockPersistentVolumes(ctx, volumeIDs)
	if err != nil {
		return err
	}
	if len(volumes) != len(volumeIDs) {
		return Invalid("数据卷已删除")
	}
	for _, volume := range volumes {
		if volume.State != "ready" {
			return Invalid("数据卷 %s 尚不可用", volume.Name)
		}
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, asset := range assets {
		if asset.TemplateId != "" && !seen[asset.TemplateId] {
			ids = append(ids, asset.TemplateId)
			seen[asset.TemplateId] = true
		}
	}
	rows, err := q.LockTemplates(ctx, ids)
	if err != nil {
		return err
	}
	if len(rows) != len(ids) {
		return Invalid("资产模板已删除，请重新选择模板")
	}
	for _, row := range rows {
		var template api.Template
		if err = json.Unmarshal(row.Definition, &template); err != nil {
			return err
		}
		if template.State != nil && *template.State == api.TemplateStateDeleting {
			return Invalid("模板 %s 正在删除", template.Name)
		}
	}
	poolIDs := []string{}
	for _, asset := range assets {
		if asset.StoragePoolId != nil && !seen["pool:"+*asset.StoragePoolId] {
			poolIDs = append(poolIDs, *asset.StoragePoolId)
			seen["pool:"+*asset.StoragePoolId] = true
		}
	}
	pools, err := q.LockStoragePools(ctx, poolIDs)
	if err != nil {
		return err
	}
	if len(pools) != len(poolIDs) {
		return Invalid("存储池已删除")
	}
	for _, pool := range pools {
		if pool.State != "ready" {
			return Invalid("存储池 %s 正在删除", pool.Name)
		}
	}
	return nil
}
func Record(row queries.Environment) (api.Environment, error) {
	result := api.Environment{Id: row.ID, Name: row.Name, ProjectId: row.ProjectID, ExternalReference: row.ExternalReference, Revision: int(row.Revision), Status: api.EnvironmentStatus(row.Status), OperationId: row.OperationID, Error: row.Error, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time}
	result.BlueprintVersionId = row.BlueprintVersionID
	if err := json.Unmarshal(row.Spec, &result.Spec); err != nil {
		return result, err
	}
	if err := json.Unmarshal(row.View, &result.View); err != nil {
		return result, err
	}
	if result.View.Positions == nil {
		p := map[string]api.Point{}
		result.View.Positions = &p
	}
	if len(row.AppliedSpec) > 0 {
		if err := json.Unmarshal(row.AppliedSpec, &result.AppliedSpec); err != nil {
			return result, err
		}
	}
	if len(row.Draft) > 0 {
		if err := json.Unmarshal(row.Draft, &result.Draft); err != nil {
			return result, err
		}
	}
	return result, nil
}
func Operation(row queries.Operation) (api.Operation, error) {
	result := api.Operation{Id: row.ID, EnvironmentId: row.EnvironmentID, Kind: row.Kind, State: api.OperationState(row.State), Phase: row.Phase, Error: row.Error, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time}
	if row.Kind == "vpn-create" || row.Kind == "vpn-revoke" {
		result.Total = 1
		if row.State == "succeeded" {
			result.Completed = 1
		}
		return result, nil
	}
	results := []api.ExecutionResult{}
	if err := json.Unmarshal(row.Results, &results); err != nil {
		return result, err
	}
	result.Results = &results
	var payload struct {
		Spec       api.EnvironmentSpec                      `json:"spec"`
		BeforeSpec *api.EnvironmentSpec                     `json:"beforeSpec"`
		Targets    []struct{ Execution api.AssetExecution } `json:"targets"`
		Updates    []struct{ Execution api.AssetExecution } `json:"updates"`
		Old        []struct{ Execution api.AssetExecution } `json:"old"`
		Template   *api.Template                            `json:"template"`
		Volume     *api.NodeVolume                          `json:"volume"`
		Recovery   *struct {
			Assets []struct{ Execution api.AssetExecution } `json:"assets"`
		} `json:"recovery"`
	}
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		return result, err
	}
	targets := map[string]string{}
	for _, group := range [][]struct{ Execution api.AssetExecution }{payload.Targets, payload.Updates, payload.Old} {
		for _, t := range group {
			if _, exists := targets[t.Execution.Asset.Id]; !exists {
				targets[t.Execution.Asset.Id] = t.Execution.InstanceId
			}
		}
	}
	if payload.Recovery != nil {
		for _, target := range payload.Recovery.Assets {
			targets[target.Execution.Asset.Id] = target.Execution.InstanceId
		}
	}
	if row.Kind == "start" || row.Phase == "queued" {
		for _, a := range payload.Spec.Assets {
			if row.AssetID == nil || *row.AssetID == a.Id {
				if _, exists := targets[a.Id]; !exists {
					targets[a.Id] = ""
				}
			}
		}
	}
	result.Total = len(targets)
	if row.Kind == "change" && payload.BeforeSpec != nil && ServiceOnly(*payload.BeforeSpec, payload.Spec) {
		services := map[string]bool{}
		for _, service := range ChangedServices(*payload.BeforeSpec, payload.Spec) {
			services[service.Id] = true
		}
		result.Total = len(services)
	}
	if payload.Template != nil || payload.Volume != nil {
		result.Total = 1
	}
	if row.State == "succeeded" {
		result.Completed = result.Total
	} else if row.State == "running" && (row.Phase == "cleanup" || row.Phase == "settle" || row.Phase == "destroy" || row.Phase == "remove-network" || row.Phase == "destroyed") {
		for _, r := range results {
			if instance, exists := targets[r.AssetId]; exists && (instance == r.InstanceId || instance == "") && r.Error == nil {
				result.Completed++
				delete(targets, r.AssetId)
			}
		}
	}
	return result, nil
}
func (s Service) Authorized(ctx context.Context, identity access.Identity, id, permission, asset string) (queries.Environment, error) {
	row, err := s.Queries.GetEnvironment(ctx, id)
	if err != nil {
		return row, err
	}
	if !identity.Allows(permission, row.ProjectID, row.ID, asset, row.OwnerID) {
		return row, access.ErrForbidden
	}
	return row, nil
}
func (s Service) Create(ctx context.Context, identity access.Identity, request api.CreateEnvironment) (api.Environment, error) {
	project := "default"
	if request.ProjectId != nil {
		project = *request.ProjectId
	}
	if !identity.Allows("compose", project, "", "", nil) {
		return api.Environment{}, access.ErrForbidden
	}
	if strings.TrimSpace(request.Name) == "" {
		return api.Environment{}, Invalid("请输入环境名称")
	}
	if request.ClientRequestId != nil {
		previous, err := s.Queries.GetEnvironmentByRequest(ctx, queries.GetEnvironmentByRequestParams{ProjectID: project, ClientRequestID: request.ClientRequestId})
		if err == nil {
			if !identity.Allows("read", previous.ProjectID, previous.ID, "", previous.OwnerID) {
				return api.Environment{}, access.ErrForbidden
			}
			return Record(previous)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return api.Environment{}, err
		}
	}
	source, err := s.CreationSpec(ctx, identity, request)
	if err != nil {
		return api.Environment{}, err
	}
	spec := source.Spec
	if source.Definition == nil {
		templates, err := Templates(ctx, s.Queries, spec.Assets)
		if err != nil {
			return api.Environment{}, err
		}
		spec, err = Normalize(spec, templates)
		if err != nil {
			return api.Environment{}, err
		}
	}
	if err = AuthorizeExternal(identity, api.EnvironmentSpec{}, spec); err != nil {
		return api.Environment{}, err
	}
	if source.Definition == nil {
		if err = AuthorizeVolumes(identity, nil, spec.Assets); err != nil {
			return api.Environment{}, err
		}
		spec, err = ResolveVolumes(ctx, s.Queries, spec)
		if err != nil {
			return api.Environment{}, err
		}
	}
	if len(Services(spec)) > 0 && !identity.Allows("access", project, "", "", nil) {
		return api.Environment{}, access.ErrForbidden
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return api.Environment{}, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return api.Environment{}, err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	var owner *string
	if source.Backup != nil {
		backup, err := q.LockBackup(ctx, source.Backup.ID)
		if err != nil {
			return api.Environment{}, err
		}
		if backup.State != "ready" {
			return api.Environment{}, Invalid("备份尚不可用")
		}
	}
	if request.BlueprintVersionId != nil {
		if _, err = q.LockBlueprintVersion(ctx, *request.BlueprintVersionId); err != nil {
			return api.Environment{}, err
		}
	}
	if source.Recovery != nil {
		point, err := q.LockRecoveryPoint(ctx, queries.LockRecoveryPointParams{ID: source.Recovery.ID, EnvironmentID: source.Recovery.EnvironmentID})
		if err != nil {
			return api.Environment{}, err
		}
		if point.State != "ready" {
			return api.Environment{}, Invalid("恢复点尚不可用")
		}
	}
	if source.Backup == nil {
		if err = ReferenceResources(ctx, q, spec.Assets); err != nil {
			return api.Environment{}, err
		}
	}
	if identity.Principal.Kind == "user" {
		owner = &identity.Principal.ID
	}
	if err = CheckVolumeUses(ctx, q, spec.Assets, ""); err != nil {
		return api.Environment{}, err
	}
	row, err := q.CreateEnvironment(ctx, queries.CreateEnvironmentParams{ID: uuid.NewString(), ProjectID: project, OwnerID: owner, Name: strings.TrimSpace(request.Name), ExternalReference: request.ExternalReference, Spec: raw, ClientRequestID: request.ClientRequestId})
	if errors.Is(err, pgx.ErrNoRows) && request.ClientRequestId != nil {
		row, err = q.GetEnvironmentByRequest(ctx, queries.GetEnvironmentByRequestParams{ProjectID: project, ClientRequestID: request.ClientRequestId})
		if err != nil {
			return api.Environment{}, err
		}
		if !identity.Allows("read", row.ProjectID, row.ID, "", row.OwnerID) {
			return api.Environment{}, access.ErrForbidden
		}
		return Record(row)
	}
	if err != nil {
		return api.Environment{}, err
	}
	if request.BlueprintVersionId != nil {
		if err = q.SetEnvironmentBlueprint(ctx, queries.SetEnvironmentBlueprintParams{ID: row.ID, BlueprintVersionID: request.BlueprintVersionId}); err != nil {
			return api.Environment{}, err
		}
		row.BlueprintVersionID = request.BlueprintVersionId
	}
	if request.BlueprintVersionId != nil || source.Definition != nil {
		row.View, err = json.Marshal(source.View)
		if err != nil {
			return api.Environment{}, err
		}
		if err = q.SaveView(ctx, queries.SaveViewParams{ID: row.ID, View: row.View}); err != nil {
			return api.Environment{}, err
		}
	}
	if source.Definition != nil || request.Run != nil && *request.Run {
		var op queries.Operation
		var createErr error
		if source.Definition != nil {
			payload, err := json.Marshal(struct {
				Spec     api.EnvironmentSpec `json:"spec"`
				Recovery json.RawMessage     `json:"recovery"`
				Run      bool                `json:"run"`
				BackupID *string             `json:"backupId,omitempty"`
			}{spec, source.Definition, request.Run != nil && *request.Run, request.BackupId})
			if err != nil {
				return api.Environment{}, err
			}
			op, createErr = submitPayload(ctx, q, row, "clone-recovery", nil, payload, request.ClientRequestId)
		} else {
			op, createErr = submit(ctx, q, row, "start", nil, spec, request.ClientRequestId)
		}
		if createErr != nil {
			return api.Environment{}, createErr
		}
		row.OperationID = &op.ID
		row.Status = "deploying"
	}
	if err = tx.Commit(ctx); err != nil {
		return api.Environment{}, err
	}
	return Record(row)
}
func submit(ctx context.Context, q *queries.Queries, row queries.Environment, kind string, asset *string, spec api.EnvironmentSpec, requestId *string) (queries.Operation, error) {
	raw, err := json.Marshal(struct {
		Spec         api.EnvironmentSpec `json:"spec"`
		BeforeStatus string              `json:"beforeStatus"`
		BeforeSpec   json.RawMessage     `json:"beforeSpec,omitempty"`
	}{spec, row.Status, row.AppliedSpec})
	if err != nil {
		return queries.Operation{}, err
	}
	return submitPayload(ctx, q, row, kind, asset, raw, requestId)
}

func submitPayload(ctx context.Context, q *queries.Queries, row queries.Environment, kind string, asset *string, raw []byte, requestId *string) (queries.Operation, error) {
	op, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: uuid.NewString(), EnvironmentID: &row.ID, ScopeKind: "environment", ScopeID: row.ID, Kind: kind, AssetID: asset, Payload: raw, ExpectedRevision: row.Revision, ClientRequestID: requestId})
	if err != nil {
		return op, err
	}
	state := "changing"
	if (kind == "start" || kind == "clone-recovery") && row.AppliedSpec == nil {
		state = "deploying"
	}
	if kind == "destroy" {
		state = "destroying"
	}
	return op, q.SetEnvironmentOperation(ctx, queries.SetEnvironmentOperationParams{ID: row.ID, OperationID: &op.ID, Status: state})
}
func existingRequest(ctx context.Context, q *queries.Queries, id string, requestID *string, kind, asset string) (api.Operation, error) {
	previous, err := q.GetOperationByRequest(ctx, queries.GetOperationByRequestParams{EnvironmentID: &id, ClientRequestID: requestID})
	if err != nil {
		return api.Operation{}, err
	}
	target := ""
	if previous.AssetID != nil {
		target = *previous.AssetID
	}
	if previous.Kind != kind || target != asset {
		return api.Operation{}, ErrConflict
	}
	return Operation(previous)
}
func (s Service) Action(ctx context.Context, identity access.Identity, id, asset string, request api.ActionRequest) (api.Operation, error) {
	permission := access.OperationPermission(string(request.Action))
	if _, err := s.Authorized(ctx, identity, id, permission, asset); err != nil {
		return api.Operation{}, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return api.Operation{}, err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	row, err := q.LockEnvironment(ctx, id)
	if err != nil {
		return api.Operation{}, err
	}
	if request.ClientRequestId != nil {
		previous, err := existingRequest(ctx, q, id, request.ClientRequestId, string(request.Action), asset)
		if err == nil {
			return previous, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return api.Operation{}, err
		}
	}
	if row.Status == "destroyed" {
		return api.Operation{}, Invalid("环境已销毁")
	}
	if request.ExpectedRevision != nil && *request.ExpectedRevision != int(row.Revision) {
		return api.Operation{}, ErrConflict
	}
	switch request.Action {
	case api.ActionRequestActionStart, api.ActionRequestActionStop, api.ActionRequestActionForceStop, api.ActionRequestActionReboot, api.ActionRequestActionSuspend, api.ActionRequestActionResume, api.ActionRequestActionRebuild, api.ActionRequestActionDestroy:
	default:
		return api.Operation{}, Invalid("未知资产动作")
	}
	record, err := Record(row)
	if err != nil {
		return api.Operation{}, err
	}
	spec := record.Spec
	if record.AppliedSpec != nil {
		spec = *record.AppliedSpec
	}
	var assetID *string
	if asset != "" {
		found := false
		for _, a := range spec.Assets {
			if a.Id == asset {
				found = true
			}
		}
		if !found {
			return api.Operation{}, pgx.ErrNoRows
		}
		assetID = &asset
	}
	if request.Action == api.ActionRequestActionStart || request.Action == api.ActionRequestActionRebuild {
		if err = ReferenceResources(ctx, q, spec.Assets); err != nil {
			return api.Operation{}, err
		}
		if err = CheckVolumeUses(ctx, q, spec.Assets, id); err != nil {
			return api.Operation{}, err
		}
	}
	op, err := submit(ctx, q, row, string(request.Action), assetID, spec, request.ClientRequestId)
	if err != nil {
		return api.Operation{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return api.Operation{}, err
	}
	return Operation(op)
}
func (s Service) Change(ctx context.Context, identity access.Identity, id string, request api.ChangeRequest) (api.ChangePreview, *api.Operation, error) {
	row, err := s.Queries.GetEnvironment(ctx, id)
	if err != nil {
		return api.ChangePreview{}, nil, err
	}
	current, err := Record(row)
	if err != nil {
		return api.ChangePreview{}, nil, err
	}
	before := current.Spec
	if current.AppliedSpec != nil {
		before = *current.AppliedSpec
	}
	request.Spec = RemoveDependentServices(before, request.Spec)
	templates, err := Templates(ctx, s.Queries, request.Spec.Assets)
	if err != nil {
		return api.ChangePreview{}, nil, err
	}
	spec, err := Normalize(request.Spec, templates)
	if err != nil {
		return api.ChangePreview{}, nil, err
	}
	if err = AuthorizeChange(identity, row, before, spec); err != nil {
		return api.ChangePreview{}, nil, err
	}
	if err = AuthorizeVolumes(identity, before.Assets, spec.Assets); err != nil {
		return api.ChangePreview{}, nil, err
	}
	spec, err = ResolveVolumes(ctx, s.Queries, spec)
	if err != nil {
		return api.ChangePreview{}, nil, err
	}
	requestedPools := map[string]string{}
	for _, asset := range spec.Assets {
		if asset.StoragePoolId != nil {
			requestedPools[asset.Id] = *asset.StoragePoolId
		}
	}
	if len(requestedPools) > 0 && current.AppliedSpec != nil {
		actual, err := s.Queries.ListRuntimeAssets(ctx, id)
		if err != nil {
			return api.ChangePreview{}, nil, err
		}
		for _, asset := range actual {
			pool, requested := requestedPools[asset.AssetID]
			if !asset.Current || !requested {
				continue
			}
			var execution api.AssetExecution
			if err = json.Unmarshal(asset.Execution, &execution); err != nil {
				return api.ChangePreview{}, nil, err
			}
			if execution.StoragePoolId == nil || *execution.StoragePoolId != pool {
				return api.ChangePreview{}, nil, Invalid("资产 %s 的磁盘移动应通过迁移完成", execution.Asset.Name)
			}
		}
	}
	if request.Apply && request.ClientRequestId != nil {
		previous, err := existingRequest(ctx, s.Queries, id, request.ClientRequestId, "change", "")
		if err == nil {
			return api.ChangePreview{}, &previous, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return api.ChangePreview{}, nil, err
		}
	}
	if request.Apply && int(row.Revision) != request.ExpectedRevision {
		return api.ChangePreview{}, nil, ErrConflict
	}
	preview := Diff(int(row.Revision), before, spec, templates)
	if !request.Apply {
		return preview, nil, nil
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return preview, nil, err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	row, err = q.LockEnvironment(ctx, id)
	if err != nil {
		return preview, nil, err
	}
	if request.ClientRequestId != nil {
		previous, err := existingRequest(ctx, q, id, request.ClientRequestId, "change", "")
		if err == nil {
			return preview, &previous, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return preview, nil, err
		}
	}
	if int(row.Revision) != request.ExpectedRevision {
		return preview, nil, ErrConflict
	}
	if row.Status == "destroyed" {
		return preview, nil, Invalid("环境已销毁")
	}
	if err = ReferenceResources(ctx, q, spec.Assets); err != nil {
		return preview, nil, err
	}
	if err = CheckVolumeUses(ctx, q, spec.Assets, id); err != nil {
		return preview, nil, err
	}
	if row.AppliedSpec == nil {
		raw, marshalErr := json.Marshal(spec)
		if marshalErr != nil {
			return preview, nil, marshalErr
		}
		if err = q.SaveDesiredSpec(ctx, queries.SaveDesiredSpecParams{ID: id, Spec: raw}); err != nil {
			return preview, nil, err
		}
		if err = q.SaveDraft(ctx, queries.SaveDraftParams{ID: id}); err != nil {
			return preview, nil, err
		}
		return preview, nil, tx.Commit(ctx)
	}
	op, err := submit(ctx, q, row, "change", nil, spec, request.ClientRequestId)
	if err != nil {
		return preview, nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return preview, nil, err
	}
	result, err := Operation(op)
	return preview, &result, err
}

func (s Service) SaveDraft(ctx context.Context, identity access.Identity, id string, input api.Draft) error {
	if _, err := s.Authorized(ctx, identity, id, "compose", ""); err != nil {
		return err
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	row, err := q.LockEnvironment(ctx, id)
	if err != nil {
		return err
	}
	if row.Status == "destroyed" {
		return Invalid("环境已销毁")
	}
	var before api.EnvironmentSpec
	current := row.Spec
	if row.AppliedSpec != nil {
		current = row.AppliedSpec
	}
	if err = json.Unmarshal(current, &before); err != nil {
		return err
	}
	if err = AuthorizeVolumes(identity, before.Assets, input.Spec.Assets); err != nil {
		return err
	}
	if err = ReferenceResources(ctx, q, input.Spec.Assets); err != nil {
		return err
	}
	if err = q.SaveDraft(ctx, queries.SaveDraftParams{ID: id, Draft: raw}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
