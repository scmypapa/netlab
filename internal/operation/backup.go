package operation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
)

type BackupDefinition struct {
	Recovery Recovery       `json:"recovery"`
	View     api.CanvasView `json:"view"`
}

func (w Worker) backupRepository(ctx context.Context, id string) (api.NodeBackupRepository, queries.Node, error) {
	row, err := w.Queries.GetBackupRepository(ctx, id)
	if err != nil {
		return api.NodeBackupRepository{}, queries.Node{}, err
	}
	nodes, err := w.Queries.GetNodeEndpoints(ctx, []string{row.NodeID})
	if err != nil {
		return api.NodeBackupRepository{}, queries.Node{}, err
	}
	if len(nodes) != 1 {
		return api.NodeBackupRepository{}, queries.Node{}, errors.New("备份仓库节点不存在")
	}
	node := queries.Node{ID: nodes[0].ID, Endpoint: nodes[0].Endpoint}
	raw, err := w.Secrets.Decrypt(row.Credentials, row.ID)
	if err != nil {
		return api.NodeBackupRepository{}, node, err
	}
	var credentials api.BackupCredentials
	if err = json.Unmarshal(raw, &credentials); err != nil {
		return api.NodeBackupRepository{}, node, err
	}
	return api.NodeBackupRepository{Location: row.Location, Password: credentials.Password, AccessKey: credentials.AccessKey, SecretKey: credentials.SecretKey, Region: credentials.Region}, node, nil
}

func (w Worker) recoverySources(ctx context.Context, p *Payload) (map[string]api.NodeRecoverySource, error) {
	result := make(map[string]api.NodeRecoverySource, len(p.Recovery.Assets))
	if p.BackupID != nil {
		row, err := w.Queries.GetBackup(ctx, *p.BackupID)
		if err != nil {
			return nil, err
		}
		repository, node, err := w.backupRepository(ctx, row.RepositoryID)
		if err != nil {
			return nil, err
		}
		var native api.NodeBackupResult
		if err = json.Unmarshal(row.Result, &native); err != nil {
			return nil, err
		}
		for _, source := range p.Recovery.Assets {
			part, ok := native.Parts[source.Execution.Asset.Id]
			if !ok {
				return nil, fmt.Errorf("备份缺少资产 %s", source.Execution.Asset.Name)
			}
			result[source.Execution.Asset.Id] = api.NodeRecoverySource{EnvironmentId: p.Recovery.EnvironmentID, NodeId: node.ID, Endpoint: node.Endpoint, Execution: source.Execution,
				Backup: &api.NodeBackupSource{Repository: repository, SnapshotId: part.SnapshotId, SizeBytes: part.SizeBytes}}
		}
		return result, nil
	}
	ids := make([]string, 0, len(p.Recovery.Assets))
	for _, source := range p.Recovery.Assets {
		ids = append(ids, source.NodeID)
	}
	rows, err := w.Queries.GetNodeEndpoints(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errPersistence, err)
	}
	origins := map[string]string{}
	for _, row := range rows {
		origins[row.ID] = row.Endpoint
	}
	for _, source := range p.Recovery.Assets {
		endpoint, ok := origins[source.NodeID]
		if !ok {
			return nil, fmt.Errorf("恢复点资产 %s 所在节点不存在", source.Execution.Asset.Name)
		}
		result[source.Execution.Asset.Id] = api.NodeRecoverySource{EnvironmentId: p.Recovery.EnvironmentID, NodeId: source.NodeID, Endpoint: endpoint, Execution: source.Execution}
	}
	return result, nil
}

func (w Worker) backup(ctx context.Context, op *queries.Operation, p *Payload) error {
	if op.ScopeKind == "backup-repository" {
		repository, node, err := w.backupRepository(ctx, op.ScopeID)
		if err != nil {
			return err
		}
		var result struct {
			ID string `json:"id"`
		}
		err = w.Client.Do(ctx, http.MethodPost, node.Endpoint, fmt.Sprintf("/node/v1/backup-repositories?initialize=%t", p.BackupInitialize), repository, &result)
		if err != nil {
			return err
		}
		if result.ID == "" {
			return errors.New("节点未返回备份仓库身份")
		}
		p.BackupNativeID = &result.ID
		return w.phase(ctx, op, p, "backup-complete")
	}
	row, err := w.Queries.GetBackup(ctx, op.ScopeID)
	if err != nil {
		return err
	}
	repository, node, err := w.backupRepository(ctx, row.RepositoryID)
	if err != nil {
		return err
	}
	if op.Kind == "delete-backup" {
		return w.Client.Do(ctx, http.MethodDelete, node.Endpoint, "/node/v1/backups/"+row.ID, repository, nil)
	}
	sources, err := w.recoverySources(ctx, &Payload{Recovery: p.Recovery})
	if err != nil {
		return err
	}
	plan := api.NodeBackupPlan{Repository: repository, RecoveryPointId: p.Recovery.ID, Definition: row.Definition, Sources: []api.NodeRecoverySource{}}
	for _, asset := range p.Recovery.Assets {
		plan.Sources = append(plan.Sources, sources[asset.Execution.Asset.Id])
	}
	var result api.NodeBackupResult
	if err = w.Client.Do(ctx, http.MethodPost, node.Endpoint, "/node/v1/backups/"+row.ID, plan, &result); err != nil {
		return err
	}
	if result.ManifestSnapshotId == "" || len(result.Parts) != len(p.Recovery.Assets) {
		return errors.New("节点未返回完整备份")
	}
	for _, asset := range p.Recovery.Assets {
		part, ok := result.Parts[asset.Execution.Asset.Id]
		if !ok || part.SnapshotId == "" || part.SizeBytes < 0 {
			return fmt.Errorf("节点未返回资产 %s 的备份", asset.Execution.Asset.Name)
		}
	}
	p.BackupResult = &result
	return w.phase(ctx, op, p, "backup-complete")
}

func (w Worker) finishBackup(ctx context.Context, q *queries.Queries, op queries.Operation, p *Payload, failure error) error {
	state := "ready"
	if failure != nil {
		state = "failed"
	}
	if op.ScopeKind == "backup-repository" {
		return q.CompleteBackupRepository(ctx, queries.CompleteBackupRepositoryParams{ID: op.ScopeID, State: state, NativeID: p.BackupNativeID})
	}
	if op.Kind == "delete-backup" {
		if failure == nil {
			return q.DeleteBackup(ctx, op.ScopeID)
		}
		return nil
	}
	var size int64
	if p.BackupResult != nil {
		for _, part := range p.BackupResult.Parts {
			size += part.SizeBytes
		}
	}
	raw, err := json.Marshal(p.BackupResult)
	if err != nil {
		return err
	}
	return q.CompleteBackup(ctx, queries.CompleteBackupParams{ID: op.ScopeID, State: state, Result: raw, SizeBytes: size})
}
