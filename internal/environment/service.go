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
	if payload.Template != nil {
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
	inputSpec, view, err := s.CreationSpec(ctx, identity, request)
	if err != nil {
		return api.Environment{}, err
	}
	templates, err := Templates(ctx, s.Queries, inputSpec.Assets)
	if err != nil {
		return api.Environment{}, err
	}
	spec, err := Normalize(inputSpec, templates)
	if err != nil {
		return api.Environment{}, err
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
	if identity.Principal.Kind == "user" {
		owner = &identity.Principal.ID
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
		row.View, err = json.Marshal(view)
		if err != nil {
			return api.Environment{}, err
		}
		if err = q.SaveView(ctx, queries.SaveViewParams{ID: row.ID, View: row.View}); err != nil {
			return api.Environment{}, err
		}
	}
	if request.Run != nil && *request.Run {
		op, createErr := submit(ctx, q, row, "start", nil, spec, request.ClientRequestId)
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
	op, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: uuid.NewString(), EnvironmentID: &row.ID, ScopeKind: "environment", ScopeID: row.ID, Kind: kind, AssetID: asset, Payload: raw, ExpectedRevision: row.Revision, ClientRequestID: requestId})
	if err != nil {
		return op, err
	}
	state := "changing"
	if kind == "start" && row.AppliedSpec == nil {
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
