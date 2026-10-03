package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/environment"
	"netlab.local/core/internal/operation"
)

func (s *Server) listRecoveryPoints(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	id := r.PathValue("id")
	if _, err := s.Environments.Authorized(r.Context(), identity, id, "read", ""); err != nil {
		return err
	}
	cursor, limit, err := pagination(r)
	if err != nil {
		return err
	}
	rows, err := s.Queries.ListRecoveryPoints(r.Context(), queries.ListRecoveryPointsParams{EnvironmentID: id, Cursor: cursor, PageLimit: limit})
	if err != nil {
		return err
	}
	result := make([]api.RecoveryPointSummary, 0, len(rows))
	for _, row := range rows {
		item := api.RecoveryPointSummary{Id: row.ID, EnvironmentId: id, Name: row.Name, Revision: int(row.Revision), State: api.RecoveryPointSummaryState(row.State),
			AssetCount: int(row.AssetCount), MemoryAssetCount: int(row.MemoryAssetCount), SizeBytes: row.SizeBytes, OperationId: &row.OperationID, Error: row.Error, CreatedAt: row.CreatedAt.Time}
		if row.State == "ready" {
			consistency := api.RecoveryConsistency(row.Consistency)
			item.Consistency = &consistency
		}
		result = append(result, item)
	}
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) captureRecoveryPoint(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	ctx, id := r.Context(), r.PathValue("id")
	if _, err := s.Environments.Authorized(ctx, identity, id, "manage", ""); err != nil {
		return err
	}
	var input api.CaptureRecoveryPoint
	if err := decode(w, r, &input); err != nil {
		return err
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return environment.Invalid("请输入恢复点名称")
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
	if int(row.Revision) != input.ExpectedRevision {
		return environment.ErrConflict
	}
	if row.Status != "running" && row.Status != "stopped" && row.Status != "suspended" {
		return environment.Invalid("请等待环境进入稳定运行状态")
	}
	recovery := operation.Recovery{ID: uuid.NewString(), EnvironmentID: id, Assets: []operation.Target{}, IncludeMemory: input.IncludeMemory}
	if err = json.Unmarshal(row.AppliedSpec, &recovery.Spec); err != nil {
		return err
	}
	assets, err := q.ListRuntimeAssets(ctx, id)
	if err != nil {
		return err
	}
	for _, asset := range assets {
		if !asset.Current {
			return httpError{http.StatusConflict, "请先完成当前环境的清理任务"}
		}
		var execution api.AssetExecution
		if err = json.Unmarshal(asset.Execution, &execution); err != nil {
			return err
		}
		recovery.Assets = append(recovery.Assets, operation.Target{NodeID: asset.NodeID, Execution: execution, State: asset.State})
	}
	if len(recovery.Assets) != len(recovery.Spec.Assets) {
		return httpError{http.StatusConflict, "运行资产与已应用配置不一致"}
	}
	// Hold the existing resource locks until the new recovery reference is committed.
	if err = environment.ReferenceResources(ctx, q, recovery.Spec.Assets); err != nil {
		return err
	}
	payload, err := json.Marshal(operation.Payload{Recovery: &recovery, Spec: recovery.Spec, BeforeStatus: row.Status})
	if err != nil {
		return err
	}
	op, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: uuid.NewString(), EnvironmentID: &id, ScopeKind: "environment", ScopeID: id,
		Kind: "capture-recovery", Payload: payload, ExpectedRevision: row.Revision})
	if err != nil {
		return err
	}
	raw, err := json.Marshal(recovery)
	if err != nil {
		return err
	}
	if err = q.CreateRecoveryPoint(ctx, queries.CreateRecoveryPointParams{ID: recovery.ID, EnvironmentID: id, Name: name, Revision: row.Revision,
		Definition: raw, AssetCount: int32(len(recovery.Assets)), OperationID: op.ID}); err != nil {
		return err
	}
	if err = q.SetEnvironmentOperation(ctx, queries.SetEnvironmentOperationParams{ID: id, OperationID: &op.ID, Status: "changing"}); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	w.Header().Set("Operation-Location", "/api/v1/operations/"+op.ID)
	return writeJSON(w, http.StatusCreated, api.RecoveryPointSummary{Id: recovery.ID, EnvironmentId: id, Name: name, Revision: int(row.Revision),
		State: api.RecoveryPointSummaryStateCapturing, AssetCount: len(recovery.Assets), OperationId: &op.ID, CreatedAt: time.Now().UTC()})
}

func (s *Server) restoreRecoveryPoint(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	ctx, id := r.Context(), r.PathValue("id")
	if _, err := s.Environments.Authorized(ctx, identity, id, "manage", ""); err != nil {
		return err
	}
	var input api.RestoreRecoveryPoint
	if err := decode(w, r, &input); err != nil {
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
	if int(row.Revision) != input.ExpectedRevision {
		return environment.ErrConflict
	}
	if row.Status == "deploying" || row.Status == "changing" || row.Status == "destroying" {
		return httpError{http.StatusConflict, "请等待当前任务完成"}
	}
	assets, err := q.ListRuntimeAssets(ctx, id)
	if err != nil {
		return err
	}
	for _, asset := range assets {
		if !asset.Current {
			return httpError{http.StatusConflict, "请先完成当前环境的清理任务"}
		}
	}
	var recovery operation.Recovery
	var backupID *string
	if requested := r.PathValue("backupId"); requested != "" {
		backup, err := q.LockBackup(ctx, requested)
		if err != nil {
			return err
		}
		if backup.EnvironmentID == nil || *backup.EnvironmentID != id {
			return pgx.ErrNoRows
		}
		if backup.State != "ready" {
			return httpError{http.StatusConflict, "备份尚不可用"}
		}
		var definition operation.BackupDefinition
		if err = json.Unmarshal(backup.Definition, &definition); err != nil {
			return err
		}
		recovery, backupID = definition.Recovery, &backup.ID
		for i := range recovery.Spec.Assets {
			recovery.Spec.Assets[i].StoragePoolId = nil
		}
		if recovery.Spec.Services != nil {
			for i := range *recovery.Spec.Services {
				(*recovery.Spec.Services)[i].ListenPort = nil
			}
		}
	} else {
		point, err := q.LockRecoveryPoint(ctx, queries.LockRecoveryPointParams{ID: r.PathValue("pointId"), EnvironmentID: id})
		if err != nil {
			return err
		}
		if point.State != "ready" {
			return httpError{http.StatusConflict, "恢复点尚不可用"}
		}
		if err = json.Unmarshal(point.Definition, &recovery); err != nil {
			return err
		}
	}
	recovery.EnvironmentID = id
	before := api.EnvironmentSpec{}
	if len(row.AppliedSpec) > 0 {
		if err = json.Unmarshal(row.AppliedSpec, &before); err != nil {
			return err
		}
	}
	if err = environment.AuthorizeExternal(identity, before, recovery.Spec); err != nil {
		return err
	}
	if err = environment.ReferenceResources(ctx, q, recovery.Spec.Assets); err != nil {
		return err
	}
	payload, err := json.Marshal(operation.Payload{Recovery: &recovery, Spec: recovery.Spec, BeforeStatus: row.Status, BackupID: backupID})
	if err != nil {
		return err
	}
	op, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: uuid.NewString(), EnvironmentID: &id, ScopeKind: "environment", ScopeID: id,
		Kind: "restore-recovery", Payload: payload, ExpectedRevision: row.Revision})
	if err != nil {
		return err
	}
	if err = q.SetEnvironmentOperation(ctx, queries.SetEnvironmentOperationParams{ID: id, OperationID: &op.ID, Status: "changing"}); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	result, err := environment.Operation(op)
	if err != nil {
		return err
	}
	w.Header().Set("Operation-Location", "/api/v1/operations/"+op.ID)
	return writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) deleteRecoveryPoint(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	ctx, id := r.Context(), r.PathValue("id")
	if _, err := s.Environments.Authorized(ctx, identity, id, "manage", ""); err != nil {
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
	if row.Status == "deploying" || row.Status == "changing" || row.Status == "destroying" {
		return httpError{http.StatusConflict, "请等待当前任务完成"}
	}
	point, err := q.LockRecoveryPoint(ctx, queries.LockRecoveryPointParams{ID: r.PathValue("pointId"), EnvironmentID: id})
	if err != nil {
		return err
	}
	if point.State == "capturing" || point.State == "deleting" {
		return httpError{http.StatusConflict, "请等待恢复点任务完成；失败任务可重试"}
	}
	inUse, err := q.RecoveryPointInUse(ctx, point.ID)
	if err != nil {
		return err
	}
	if inUse {
		return httpError{http.StatusConflict, "恢复点正在使用中"}
	}
	var recovery operation.Recovery
	if err = json.Unmarshal(point.Definition, &recovery); err != nil {
		return err
	}
	payload, err := json.Marshal(operation.Payload{Recovery: &recovery, Spec: recovery.Spec, BeforeStatus: row.Status})
	if err != nil {
		return err
	}
	op, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: uuid.NewString(), EnvironmentID: &id, ScopeKind: "environment", ScopeID: id,
		Kind: "delete-recovery", Payload: payload, ExpectedRevision: row.Revision})
	if err != nil {
		return err
	}
	if err = q.MarkRecoveryDeleting(ctx, queries.MarkRecoveryDeletingParams{ID: point.ID, OperationID: op.ID}); err != nil {
		return err
	}
	if err = q.SetRecoveryDeleteOperation(ctx, queries.SetRecoveryDeleteOperationParams{ID: id, OperationID: &op.ID}); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	result, err := environment.Operation(op)
	if err != nil {
		return err
	}
	w.Header().Set("Operation-Location", "/api/v1/operations/"+op.ID)
	return writeJSON(w, http.StatusAccepted, result)
}
