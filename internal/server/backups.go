package server

import (
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/environment"
	"netlab.local/core/internal/operation"
)

func (s *Server) listBackupRepositories(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	rows, err := s.Queries.ListBackupRepositories(r.Context())
	if err != nil {
		return err
	}
	result := make([]api.BackupRepository, 0, len(rows))
	for _, row := range rows {
		repo := row.BackupRepository
		item := api.BackupRepository{Id: repo.ID, Name: repo.Name, NodeId: repo.NodeID, State: api.BackupRepositoryState(repo.State)}
		if identity.Administrator() {
			item.Location, item.OperationId, item.Error = &repo.Location, repo.OperationID, row.Error
		}
		result = append(result, item)
	}
	return writeJSON(w, http.StatusOK, result)
}

func backupLocation(location string) error {
	if path.IsAbs(location) {
		return nil
	}
	u, err := url.Parse(strings.TrimPrefix(location, "s3:"))
	if !strings.HasPrefix(location, "s3:") || err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") == "" {
		return environment.Invalid("仓库地址应为节点绝对目录或 s3:https://服务地址/存储桶/目录")
	}
	return nil
}

func (s *Server) createBackupRepository(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	var input api.CreateBackupRepository
	if err := decode(w, r, &input); err != nil {
		return err
	}
	input.Name, input.Location = strings.TrimSpace(input.Name), strings.TrimSpace(input.Location)
	if input.Name == "" {
		return environment.Invalid("请输入仓库名称")
	}
	if err := backupLocation(input.Location); err != nil {
		return err
	}
	initialize := input.Initialize == nil || *input.Initialize
	credentials := api.BackupCredentials{AccessKey: input.AccessKey, SecretKey: input.SecretKey, Region: input.Region}
	if input.Password != nil {
		credentials.Password = *input.Password
	} else if initialize {
		credentials.Password = rand.Text()
	}
	if credentials.Password == "" {
		return environment.Invalid("连接现有仓库需要仓库密码")
	}
	ctx := r.Context()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	if _, err = q.LockNode(ctx, input.NodeId); err != nil {
		return err
	}
	id := uuid.NewString()
	secret, err := json.Marshal(credentials)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(operation.Payload{BackupInitialize: initialize})
	if err != nil {
		return err
	}
	op, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: uuid.NewString(), ScopeKind: "backup-repository", ScopeID: id, Kind: "connect-backup-repository", Payload: payload})
	if err != nil {
		return err
	}
	if err = q.CreateBackupRepository(ctx, queries.CreateBackupRepositoryParams{ID: id, NodeID: input.NodeId, Name: input.Name, Location: input.Location, Credentials: s.Secrets.Encrypt(secret, id), OperationID: &op.ID}); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	w.Header().Set("Operation-Location", "/api/v1/operations/"+op.ID)
	return writeJSON(w, http.StatusCreated, api.BackupRepository{Id: id, Name: input.Name, NodeId: input.NodeId, Location: &input.Location, State: api.BackupRepositoryStateConnecting, OperationId: &op.ID})
}

func (s *Server) backupRepositoryCredentials(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	row, err := s.Queries.GetBackupRepository(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	raw, err := s.Secrets.Decrypt(row.Credentials, row.ID)
	if err != nil {
		return err
	}
	var result api.BackupCredentials
	if err = json.Unmarshal(raw, &result); err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) deleteBackupRepository(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	ctx := r.Context()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	row, err := q.LockBackupRepository(ctx, r.PathValue("id"))
	if err != nil {
		return err
	}
	if row.OperationID != nil {
		op, err := q.LockOperation(ctx, *row.OperationID)
		if err != nil {
			return err
		}
		if op.State == "queued" || op.State == "running" {
			return httpError{http.StatusConflict, "仓库正在连接"}
		}
	}
	if _, err = q.LockRepositoryBackups(ctx, row.ID); err != nil {
		return err
	}
	inUse, err := q.BackupRepositoryInUse(ctx, row.ID)
	if err != nil {
		return err
	}
	if inUse {
		return httpError{http.StatusConflict, "仓库仍有备份"}
	}
	if err = q.DeleteRepositoryCatalog(ctx, row.ID); err != nil {
		return err
	}
	if err = q.DeleteBackupRepository(ctx, row.ID); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func backupSummary(row queries.Backup, failure *string) api.BackupSummary {
	return api.BackupSummary{Id: row.ID, EnvironmentId: row.EnvironmentID, RepositoryId: row.RepositoryID, Name: row.Name,
		State: api.BackupSummaryState(row.State), SizeBytes: row.SizeBytes, OperationId: &row.OperationID, Error: failure, CreatedAt: row.CreatedAt.Time}
}

func (s *Server) listBackups(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	ctx, id, repository := r.Context(), r.PathValue("id"), r.PathValue("repositoryId")
	if repository != "" {
		if err := requireAdministrator(identity); err != nil {
			return err
		}
		if _, err := s.Queries.GetBackupRepository(ctx, repository); err != nil {
			return err
		}
	} else {
		if _, err := s.Environments.Authorized(ctx, identity, id, "read", ""); err != nil {
			return err
		}
	}
	cursor, limit, err := pagination(r)
	if err != nil {
		return err
	}
	rows, err := s.Queries.ListBackups(ctx, queries.ListBackupsParams{EnvironmentID: id, RepositoryID: repository, Cursor: cursor, PageLimit: limit})
	if err != nil {
		return err
	}
	result := make([]api.BackupSummary, 0, len(rows))
	for _, row := range rows {
		result = append(result, api.BackupSummary{Id: row.ID, EnvironmentId: row.EnvironmentID, RepositoryId: row.RepositoryID, Name: row.Name,
			State: api.BackupSummaryState(row.State), SizeBytes: row.SizeBytes, OperationId: &row.OperationID, Error: row.Error, CreatedAt: row.CreatedAt.Time})
	}
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) createBackup(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	ctx, envID := r.Context(), r.PathValue("id")
	if _, err := s.Environments.Authorized(ctx, identity, envID, "manage", ""); err != nil {
		return err
	}
	var input api.CreateBackup
	if err := decode(w, r, &input); err != nil {
		return err
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		return environment.Invalid("请输入备份名称")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	point, err := q.LockRecoveryPoint(ctx, queries.LockRecoveryPointParams{ID: input.RecoveryPointId, EnvironmentID: envID})
	if err != nil {
		return err
	}
	if point.State != "ready" {
		return httpError{http.StatusConflict, "恢复点尚不可用"}
	}
	repository, err := q.LockBackupRepository(ctx, input.RepositoryId)
	if err != nil {
		return err
	}
	if repository.State != "ready" {
		return httpError{http.StatusConflict, "备份仓库尚不可用"}
	}
	var definition operation.BackupDefinition
	if err = json.Unmarshal(point.Definition, &definition.Recovery); err != nil {
		return err
	}
	env, err := q.GetEnvironment(ctx, envID)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(env.View, &definition.View); err != nil {
		return err
	}
	raw, err := json.Marshal(definition)
	if err != nil {
		return err
	}
	id := uuid.NewString()
	payload, err := json.Marshal(operation.Payload{Recovery: &definition.Recovery, BackupID: &id})
	if err != nil {
		return err
	}
	op, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: uuid.NewString(), EnvironmentID: &envID, ScopeKind: "backup", ScopeID: id, Kind: "create-backup", Payload: payload})
	if err != nil {
		return err
	}
	if err = q.CreateBackup(ctx, queries.CreateBackupParams{ID: id, EnvironmentID: &envID, RepositoryID: repository.ID, Name: input.Name, Definition: raw, OperationID: op.ID}); err != nil {
		return err
	}
	row, err := q.GetBackup(ctx, id)
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	w.Header().Set("Operation-Location", "/api/v1/operations/"+op.ID)
	return writeJSON(w, http.StatusCreated, backupSummary(row, nil))
}

func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	ctx, envID, repository := r.Context(), r.PathValue("id"), r.PathValue("repositoryId")
	if repository != "" {
		if err := requireAdministrator(identity); err != nil {
			return err
		}
	} else {
		if _, err := s.Environments.Authorized(ctx, identity, envID, "manage", ""); err != nil {
			return err
		}
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	row, err := q.LockBackup(ctx, r.PathValue("backupId"))
	if err != nil {
		return err
	}
	if repository != "" && row.RepositoryID != repository || repository == "" && (row.EnvironmentID == nil || *row.EnvironmentID != envID) {
		return pgx.ErrNoRows
	}
	op, err := q.LockOperation(ctx, row.OperationID)
	if err != nil {
		return err
	}
	if op.State == "queued" || op.State == "running" {
		return httpError{http.StatusConflict, "备份任务尚未完成"}
	}
	inUse, err := q.BackupInUse(ctx, row.ID)
	if err != nil {
		return err
	}
	if inUse {
		return httpError{http.StatusConflict, "备份正在恢复"}
	}
	payload, err := json.Marshal(operation.Payload{BackupID: &row.ID})
	if err != nil {
		return err
	}
	op, err = q.CreateOperation(ctx, queries.CreateOperationParams{ID: uuid.NewString(), EnvironmentID: row.EnvironmentID, ScopeKind: "backup", ScopeID: row.ID, Kind: "delete-backup", Payload: payload})
	if err != nil {
		return err
	}
	if err = q.MarkBackupDeleting(ctx, queries.MarkBackupDeletingParams{ID: row.ID, OperationID: op.ID}); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	result, err := environment.Operation(op)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) refreshBackupRepository(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	ctx := r.Context()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	row, err := q.LockBackupRepository(ctx, r.PathValue("id"))
	if err != nil {
		return err
	}
	if row.OperationID != nil {
		op, err := q.LockOperation(ctx, *row.OperationID)
		if err != nil {
			return err
		}
		if op.State == "queued" || op.State == "running" {
			return httpError{http.StatusConflict, "仓库任务尚未完成"}
		}
	}
	op, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: uuid.NewString(), ScopeKind: "backup-repository", ScopeID: row.ID, Kind: "connect-backup-repository", Payload: []byte(`{}`)})
	if err != nil {
		return err
	}
	if err = q.MarkBackupRepositoryConnecting(ctx, queries.MarkBackupRepositoryConnectingParams{ID: row.ID, OperationID: &op.ID}); err != nil {
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
