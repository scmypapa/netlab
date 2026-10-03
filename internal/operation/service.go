package operation

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/environment"
)

type Service struct {
	Pool    *pgxpool.Pool
	Queries *queries.Queries
}

func Retryable(identity access.Identity, row queries.Operation, environment queries.Environment) bool {
	if row.State != "failed" && row.State != "partially_applied" {
		return false
	}
	if row.EnvironmentID == nil {
		return identity.Administrator()
	}
	asset := ""
	if row.AssetID != nil {
		asset = *row.AssetID
	}
	return environment.OperationID != nil && *environment.OperationID == row.ID && authorizedOperation(identity, row, environment, asset) == nil
}

func authorizedOperation(identity access.Identity, row queries.Operation, runtime queries.Environment, asset string) error {
	if row.Kind == "capture-template" && !identity.Administrator() {
		return access.ErrForbidden
	}
	runtime.ID = *row.EnvironmentID
	if row.Kind == "change" {
		var p Payload
		if err := json.Unmarshal(row.Payload, &p); err != nil {
			return err
		}
		if p.BeforeSpec != nil {
			return environment.AuthorizeChange(identity, runtime, *p.BeforeSpec, p.Spec)
		}
	}
	if !identity.Allows(access.OperationPermission(row.Kind), runtime.ProjectID, *row.EnvironmentID, asset, runtime.OwnerID) {
		return access.ErrForbidden
	}
	return nil
}

func Readable(identity access.Identity, row queries.Operation, runtime queries.Environment) bool {
	asset := ""
	if row.AssetID != nil {
		asset = *row.AssetID
	}
	if identity.Allows("read", runtime.ProjectID, runtime.ID, asset, runtime.OwnerID) {
		return true
	}
	if row.Kind == "vpn-create" || row.Kind == "vpn-revoke" {
		return identity.Allows("access", runtime.ProjectID, runtime.ID, "", runtime.OwnerID)
	}
	if row.Kind != "change" {
		return false
	}
	var p Payload
	if json.Unmarshal(row.Payload, &p) != nil || p.BeforeSpec == nil || !environment.ServiceOnly(*p.BeforeSpec, p.Spec) {
		return false
	}
	assets := environment.ChangedServiceAssets(*p.BeforeSpec, p.Spec)
	if len(assets) == 0 {
		return false
	}
	for id := range assets {
		if !identity.Allows("read", runtime.ProjectID, runtime.ID, id, runtime.OwnerID) {
			return false
		}
	}
	return true
}

func (s Service) Retry(ctx context.Context, identity access.Identity, id string) (api.Operation, error) {
	row, err := s.Queries.GetOperation(ctx, id)
	if err != nil {
		return api.Operation{}, err
	}
	if row.EnvironmentID == nil {
		if !identity.Administrator() {
			return api.Operation{}, access.ErrForbidden
		}
	} else {
		runtime, readErr := s.Queries.GetEnvironment(ctx, *row.EnvironmentID)
		if readErr != nil {
			return api.Operation{}, readErr
		}
		asset := ""
		if row.AssetID != nil {
			asset = *row.AssetID
		}
		if err = authorizedOperation(identity, row, runtime, asset); err != nil {
			return api.Operation{}, err
		}
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return api.Operation{}, err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	revision := row.ExpectedRevision
	if row.EnvironmentID != nil {
		e, readErr := q.LockEnvironment(ctx, *row.EnvironmentID)
		if readErr != nil {
			return api.Operation{}, readErr
		}
		if e.OperationID == nil || *e.OperationID != id {
			return api.Operation{}, environment.ErrConflict
		}
		revision = e.Revision
	}
	row, err = q.LockOperation(ctx, id)
	if err != nil {
		return api.Operation{}, err
	}
	if row.State != "failed" && row.State != "partially_applied" {
		return api.Operation{}, environment.Invalid("当前任务不需要重试")
	}
	var p Payload
	if err = json.Unmarshal(row.Payload, &p); err != nil {
		return api.Operation{}, err
	}
	if row.Kind == "change" {
		assets := append([]api.Asset{}, p.Spec.Assets...)
		if p.BeforeSpec != nil {
			assets = append(assets, p.BeforeSpec.Assets...)
		}
		if err = environment.ReferenceResources(ctx, q, assets); err != nil {
			return api.Operation{}, err
		}
	}
	if p.Template != nil {
		templateRow, err := q.LockTemplate(ctx, p.Template.Id)
		if err != nil {
			return api.Operation{}, err
		}
		var template api.Template
		if err = json.Unmarshal(templateRow.Definition, &template); err != nil {
			return api.Operation{}, err
		}
		if template.State != nil && *template.State == api.TemplateStateDeleting && row.Kind != "delete-template" {
			return api.Operation{}, environment.Invalid("模板正在删除")
		}
		state := api.TemplateStateImporting
		if row.Kind == "delete-template" {
			state = api.TemplateStateDeleting
		}
		template.State, template.Error, template.OperationId = &state, nil, &id
		raw, err := json.Marshal(template)
		if err != nil {
			return api.Operation{}, err
		}
		if err = q.UpdateTemplate(ctx, queries.UpdateTemplateParams{ID: template.Id, Definition: raw}); err != nil {
			return api.Operation{}, err
		}
	}
	phase := row.Phase
	if p.Recovery != nil && row.Kind == "capture-recovery" {
		if _, err = q.LockRecoveryPoint(ctx, queries.LockRecoveryPointParams{ID: p.Recovery.ID, EnvironmentID: *row.EnvironmentID}); err != nil {
			return api.Operation{}, err
		}
		if err = q.MarkRecoveryCapturing(ctx, p.Recovery.ID); err != nil {
			return api.Operation{}, err
		}
		if phase == "recovery-complete" {
			p.Failure, p.Recovery.Bytes, phase = nil, 0, "recovery-reset"
		}
	}
	if phase == "rolled-back" || phase == "queued" || row.Kind == "prepare-template" {
		p = Payload{Spec: p.Spec, BeforeStatus: p.BeforeStatus, Template: p.Template, TemplateCredentials: p.TemplateCredentials, TemplateCapture: p.TemplateCapture, BeforeSpec: p.BeforeSpec, VPNChange: p.VPNChange, StoragePool: p.StoragePool, Recovery: p.Recovery}
		phase = "queued"
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return api.Operation{}, err
	}
	row, err = q.RetryOperation(ctx, queries.RetryOperationParams{ID: id, Phase: phase, Payload: raw, ExpectedRevision: revision})
	if err != nil {
		return api.Operation{}, err
	}
	if row.EnvironmentID != nil {
		if row.Kind == "delete-recovery" {
			err = q.SetRecoveryDeleteOperation(ctx, queries.SetRecoveryDeleteOperationParams{ID: *row.EnvironmentID, OperationID: &id})
		} else {
			state := "changing"
			if row.Kind == "destroy" {
				state = "destroying"
			}
			err = q.SetEnvironmentOperation(ctx, queries.SetEnvironmentOperationParams{ID: *row.EnvironmentID, OperationID: &id, Status: state})
		}
		if err != nil {
			return api.Operation{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return api.Operation{}, err
	}
	return environment.Operation(row)
}
