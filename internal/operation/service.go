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

func (s Service) Retry(ctx context.Context, identity access.Identity, id string) (api.Operation, error) {
	row, err := s.Queries.GetOperation(ctx, id)
	if err != nil {
		return api.Operation{}, err
	}
	var envService environment.Service
	if row.EnvironmentID == nil {
		if !identity.Administrator() {
			return api.Operation{}, access.ErrForbidden
		}
	} else {
		envService = environment.Service{Pool: s.Pool, Queries: s.Queries}
		permission, asset := access.OperationPermission(row.Kind), ""
		if row.AssetID != nil {
			asset = *row.AssetID
		}
		if _, err = envService.Authorized(ctx, identity, *row.EnvironmentID, permission, asset); err != nil {
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
	phase := row.Phase
	if phase == "rolled-back" || phase == "queued" || row.Kind == "prepare-template" {
		p = Payload{Spec: p.Spec, BeforeStatus: p.BeforeStatus, Template: p.Template}
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
		state := "changing"
		if row.Kind == "destroy" {
			state = "destroying"
		}
		if err = q.SetEnvironmentOperation(ctx, queries.SetEnvironmentOperationParams{ID: *row.EnvironmentID, OperationID: &id, Status: state}); err != nil {
			return api.Operation{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return api.Operation{}, err
	}
	return environment.Operation(row)
}
