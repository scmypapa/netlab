package server

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/operation"
	"netlab.local/core/internal/secret"
)

func testBackupsAPI(t *testing.T, ctx context.Context, s *Server, admin string, call func(string, string, string, any, int) []byte) {
	t.Run("backup API authorization, source references and retry scopes", func(t *testing.T) {
		var err error
		// Earlier contract cases accept tasks without starting a worker.
		if _, err = s.Pool.Exec(ctx, "UPDATE operations SET state='succeeded' WHERE state='queued'"); err != nil {
			t.Fatal(err)
		}
		s.Secrets, err = secret.OpenFile(filepath.Join(t.TempDir(), "secret.key"))
		if err != nil {
			t.Fatal(err)
		}
		nodeID := uuid.NewString()
		if err = s.Queries.PutNode(ctx, queries.PutNodeParams{ID: nodeID, Name: "Backup contract", Endpoint: "https://backup.invalid", Info: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
		identity, err := s.Access.Authenticate(ctx, admin)
		if err != nil {
			t.Fatal(err)
		}
		spec := api.EnvironmentSpec{Assets: []api.Asset{}, Networks: []api.Network{}}
		env, err := s.Environments.Create(ctx, identity, api.CreateEnvironment{Name: "Backup contract", Spec: &spec})
		if err != nil {
			t.Fatal(err)
		}
		var reader api.IssuedServiceToken
		json.Unmarshal(call("POST", "/service-tokens", admin, api.CreateServiceToken{Name: "Backup reader", Grants: []api.ScopeGrant{{ScopeKind: "environment", ScopeId: env.Id, Permissions: []api.Permission{"read"}}}}, 201), &reader)
		call("POST", "/backup-repositories", reader.Token, nil, 403)
		call("POST", "/backup-repositories", admin, api.CreateBackupRepository{Name: "Bad", NodeId: nodeID, Location: "s3:https://user:password@example.test/bucket"}, 400)
		var repository api.BackupRepository
		if err = json.Unmarshal(call("POST", "/backup-repositories", admin, api.CreateBackupRepository{Name: "Backups", NodeId: nodeID, Location: "/mnt/backups"}, 201), &repository); err != nil {
			t.Fatal(err)
		}
		call("DELETE", "/backup-repositories/"+repository.Id, admin, nil, 409)
		call("GET", "/backup-repositories/"+repository.Id+"/credentials", reader.Token, nil, 403)
		var credentials api.BackupCredentials
		if err = json.Unmarshal(call("GET", "/backup-repositories/"+repository.Id+"/credentials", admin, nil, 200), &credentials); err != nil || credentials.Password == "" {
			t.Fatal("repository credentials missing", err)
		}
		var visible []api.BackupRepository
		if err = json.Unmarshal(call("GET", "/backup-repositories", reader.Token, nil, 200), &visible); err != nil {
			t.Fatal(err)
		}
		for _, repo := range visible {
			if repo.Location != nil || repo.Error != nil {
				t.Fatal("repository details leaked")
			}
		}
		if _, err = s.Pool.Exec(ctx, "UPDATE operations SET state='failed' WHERE id=$1", *repository.OperationId); err != nil {
			t.Fatal(err)
		}
		var task api.Operation
		if err = json.Unmarshal(call("GET", "/operations/"+*repository.OperationId, admin, nil, 200), &task); err != nil || !task.Retryable {
			t.Fatal("connection retry missing", err)
		}
		call("POST", "/operations/"+*repository.OperationId+"/retry", admin, nil, 202)
		if _, err = s.Pool.Exec(ctx, "UPDATE operations SET state='succeeded' WHERE id=$1", *repository.OperationId); err != nil {
			t.Fatal(err)
		}
		if err = s.Queries.CompleteBackupRepository(ctx, queries.CompleteBackupRepositoryParams{ID: repository.Id, State: "ready"}); err != nil {
			t.Fatal(err)
		}
		pointID, pointOp := uuid.NewString(), uuid.NewString()
		recovery := operation.Recovery{EnvironmentID: env.Id, ID: pointID, Spec: spec, Assets: []operation.Target{}}
		raw, _ := json.Marshal(recovery)
		if _, err = s.Queries.CreateOperation(ctx, queries.CreateOperationParams{ID: pointOp, EnvironmentID: &env.Id, ScopeKind: "environment", ScopeID: env.Id, Kind: "capture-recovery", Payload: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
		if err = s.Queries.CreateRecoveryPoint(ctx, queries.CreateRecoveryPointParams{ID: pointID, EnvironmentID: env.Id, Name: "Source", Revision: 1, Definition: raw, OperationID: pointOp}); err != nil {
			t.Fatal(err)
		}
		if err = s.Queries.CompleteRecoveryPoint(ctx, queries.CompleteRecoveryPointParams{ID: pointID, State: "ready", Definition: raw}); err != nil {
			t.Fatal(err)
		}
		if _, err = s.Pool.Exec(ctx, "UPDATE operations SET state='succeeded' WHERE id=$1", pointOp); err != nil {
			t.Fatal(err)
		}
		request := api.CreateBackup{Name: "Durable", RepositoryId: repository.Id, RecoveryPointId: pointID}
		path := "/environments/" + env.Id + "/backups"
		call("POST", path, reader.Token, request, 403)
		var backup api.BackupSummary
		if err = json.Unmarshal(call("POST", path, admin, request, 201), &backup); err != nil {
			t.Fatal(err)
		}
		owner := uuid.NewString()
		claimed, err := s.Queries.ClaimOperation(ctx, &owner)
		if err != nil || claimed.ID != *backup.OperationId {
			t.Fatal("backup claim identity", err)
		}
		current, err := s.Queries.GetEnvironment(ctx, env.Id)
		if err != nil || current.OperationID != nil || current.Status != "draft" {
			t.Fatal("backup occupied environment operation", err)
		}
		inUse, err := s.Queries.RecoveryPointInUse(ctx, pointID)
		if err != nil || !inUse {
			t.Fatal("backup did not protect source", err)
		}
		call("DELETE", "/backup-repositories/"+repository.Id, admin, nil, 409)
		if _, err = s.Pool.Exec(ctx, "UPDATE operations SET state='failed' WHERE id=$1", *backup.OperationId); err != nil {
			t.Fatal(err)
		}
		if inUse, err = s.Queries.RecoveryPointInUse(ctx, pointID); err != nil || !inUse {
			t.Fatal("failed backup lost source", err)
		}
		if inUse, err = s.Queries.RecoveryPointInUse(ctx, "unrelated"); err != nil || inUse {
			t.Fatal("failed backup locked unrelated source", err)
		}
		if err = json.Unmarshal(call("GET", "/operations/"+*backup.OperationId, admin, nil, 200), &task); err != nil || !task.Retryable {
			t.Fatal("backup retry missing", err)
		}
		call("POST", "/operations/"+*backup.OperationId+"/retry", reader.Token, nil, 403)
		call("POST", "/operations/"+*backup.OperationId+"/retry", admin, nil, 202)
		current, err = s.Queries.GetEnvironment(ctx, env.Id)
		if err != nil || current.OperationID != nil {
			t.Fatal("backup retry occupied environment", err)
		}
		if _, err = s.Pool.Exec(ctx, "UPDATE operations SET state='succeeded' WHERE id=$1", *backup.OperationId); err != nil {
			t.Fatal(err)
		}
		if err = s.Queries.CompleteBackup(ctx, queries.CompleteBackupParams{ID: backup.Id, State: "ready", Result: []byte(`{"manifestSnapshotId":"fixture","parts":{}}`)}); err != nil {
			t.Fatal(err)
		}
		if err = s.Queries.DeleteRecoveryPoint(ctx, pointID); err != nil {
			t.Fatal(err)
		}
		var clone api.Environment
		if err = json.Unmarshal(call("POST", "/environments", admin, api.CreateEnvironment{Name: "Backup clone", BackupId: &backup.Id}, 201), &clone); err != nil {
			t.Fatal(err)
		}
		row, err := s.Queries.GetOperation(ctx, *clone.OperationId)
		if err != nil {
			t.Fatal(err)
		}
		var payload operation.Payload
		if err = json.Unmarshal(row.Payload, &payload); err != nil || payload.BackupID == nil || *payload.BackupID != backup.Id || payload.Recovery == nil {
			t.Fatal("backup clone lost its source", err)
		}
		call("DELETE", path+"/"+backup.Id, admin, nil, 409)
		if _, err = s.Pool.Exec(ctx, "UPDATE operations SET state='failed',phase='rolled-back' WHERE id=$1", row.ID); err != nil {
			t.Fatal(err)
		}
		call("POST", "/operations/"+row.ID+"/retry", admin, nil, 202)
		if _, err = s.Pool.Exec(ctx, "UPDATE operations SET state='failed',phase='rolled-back' WHERE id=$1", row.ID); err != nil {
			t.Fatal(err)
		}
		call("DELETE", path+"/"+backup.Id, reader.Token, nil, 403)
		call("DELETE", path+"/"+backup.Id, admin, nil, 202)
		call("POST", path+"/"+backup.Id+"/restore", admin, api.RestoreRecoveryPoint{ExpectedRevision: 1}, 409)
	})
}
