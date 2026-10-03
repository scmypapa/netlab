//go:build linux

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"netlab.local/core/api"
	"netlab.local/core/internal/backup"
)

func (e *Engine) backupStore(repository api.NodeBackupRepository) backup.Store {
	s := backup.Store{Location: repository.Location, Password: repository.Password, CacheDirectory: filepath.Join(e.cfg.DataDir, "restic-cache")}
	if repository.AccessKey != nil {
		s.AccessKey = *repository.AccessKey
	}
	if repository.SecretKey != nil {
		s.SecretKey = *repository.SecretKey
	}
	if repository.Region != nil {
		s.Region = *repository.Region
	}
	return s
}

func (e *Engine) ConnectBackupRepository(ctx context.Context, repository api.NodeBackupRepository, initialize bool) (string, error) {
	return e.backupStore(repository).Connect(ctx, initialize)
}

func (e *Engine) BackupRecovery(ctx context.Context, id string, plan api.NodeBackupPlan) (api.NodeBackupResult, error) {
	unlock := e.lock("backup:" + id)
	defer unlock()
	select {
	case e.ioSlots <- struct{}{}:
	case <-ctx.Done():
		return api.NodeBackupResult{}, ctx.Err()
	}
	defer func() { <-e.ioSlots }()
	s := e.backupStore(plan.Repository)
	group := "netlab:backup:" + id
	result := api.NodeBackupResult{Parts: map[string]api.NodeBackupPart{}}
	for _, source := range plan.Sources {
		reader, size, err := e.openRecoverySource(ctx, plan.RecoveryPointId, source)
		if err != nil {
			return result, fmt.Errorf("asset %s: %w", source.Execution.Asset.Name, err)
		}
		snapshot, backupErr := s.Put(ctx, group, source.Execution.Asset.Id, backupAssetName(source.Execution.Asset.Id), size, reader)
		if err = errors.Join(backupErr, reader.Close()); err != nil {
			return result, fmt.Errorf("asset %s: %w", source.Execution.Asset.Name, err)
		}
		result.Parts[source.Execution.Asset.Id] = api.NodeBackupPart{SnapshotId: snapshot.ID, SizeBytes: size}
	}
	manifest, err := json.Marshal(struct {
		Definition json.RawMessage               `json:"definition"`
		Parts      map[string]api.NodeBackupPart `json:"parts"`
	}{plan.Definition, result.Parts})
	if err != nil {
		return result, err
	}
	snapshot, err := s.Put(ctx, group, "manifest", "/manifest.json", int64(len(manifest)), bytes.NewReader(manifest))
	result.ManifestSnapshotId = snapshot.ID
	return result, err
}

func (e *Engine) DeleteBackup(ctx context.Context, id string, repository api.NodeBackupRepository) error {
	unlock := e.lock("backup:" + id)
	defer unlock()
	return e.backupStore(repository).Delete(ctx, "netlab:backup:"+id)
}

func backupAssetName(asset string) string { return "/" + asset + ".tar" }

func (e *Engine) OpenBackupArtifact(ctx context.Context, asset string, source api.NodeBackupSource) (io.ReadCloser, int64) {
	ctx, cancel := context.WithCancel(ctx)
	reader, writer := io.Pipe()
	go func() {
		err := e.backupStore(source.Repository).Dump(ctx, source.SnapshotId, backupAssetName(asset), writer)
		writer.CloseWithError(err)
		cancel()
	}()
	return &backupReader{PipeReader: reader, cancel: cancel}, source.SizeBytes
}

type backupReader struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (r *backupReader) Close() error {
	r.cancel()
	return r.PipeReader.Close()
}

func (e *Engine) openRecoverySource(ctx context.Context, point string, source api.NodeRecoverySource) (io.ReadCloser, int64, error) {
	if source.NodeId == e.cfg.ID {
		if source.Backup != nil {
			reader, size := e.OpenBackupArtifact(ctx, source.Execution.Asset.Id, *source.Backup)
			return reader, size, nil
		}
		return e.OpenRecoveryArtifact(source.EnvironmentId, point, source.Execution)
	}
	path := fmt.Sprintf("/node/v1/environments/%s/recovery-points/%s/assets/%s/artifact", source.EnvironmentId, point, url.PathEscape(source.Execution.Asset.Id))
	var body any = source.Execution
	if source.Backup != nil {
		path, body = "/node/v1/backups/assets/"+url.PathEscape(source.Execution.Asset.Id)+"/artifact", source.Backup
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(source.Endpoint, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, err
	}
	response, err := e.cfg.ArtifactHTTP.Do(request)
	if err != nil {
		return nil, 0, err
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		message, readErr := io.ReadAll(io.LimitReader(response.Body, 16384))
		return nil, 0, errors.Join(fmt.Errorf("recovery artifact: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message))), readErr)
	}
	return response.Body, response.ContentLength, nil
}
