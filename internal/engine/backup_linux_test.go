//go:build linux

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"netlab.local/core/api"
)

func TestResticRecoveryTransfer(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic is not installed")
	}
	ctx := context.Background()
	local := &Engine{cfg: Config{ID: uuid.NewString(), DataDir: t.TempDir()}, locks: map[string]*objectLock{}, ioSlots: make(chan struct{}, 1)}
	remote := &Engine{cfg: Config{ID: uuid.NewString(), DataDir: t.TempDir()}}
	env, point, backupID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	content := bytes.Repeat([]byte("immutable recovery data\n"), 1<<12)
	assets := []api.AssetExecution{
		{InstanceId: uuid.NewString(), Asset: api.Asset{Id: "local", Name: "Local archive"}},
		{InstanceId: uuid.NewString(), Asset: api.Asset{Id: "remote", Name: "Remote archive"}},
	}
	for i, node := range []*Engine{local, remote} {
		directory := recoveryDirectory(node.cfg.DataDir, point, assets[i])
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
		if err := writeRecoveryJSON(filepath.Join(directory, "manifest.json"), recoveryManifest{EnvironmentID: env, PointID: point, Execution: assets[i]}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "data.bin"), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	truncate := true
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/node/v1/backups/assets/local/artifact" {
			var source api.NodeBackupSource
			if err := json.NewDecoder(r.Body).Decode(&source); err != nil {
				t.Error(err)
				return
			}
			reader, size := local.OpenBackupArtifact(r.Context(), "local", source)
			defer reader.Close()
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			io.Copy(w, reader)
			return
		}
		var execution api.AssetExecution
		if err := json.NewDecoder(r.Body).Decode(&execution); err != nil {
			t.Error(err)
			return
		}
		reader, size, err := remote.OpenRecoveryArtifact(env, point, execution)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		defer reader.Close()
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		if truncate {
			io.CopyN(w, reader, size/2)
		} else {
			io.Copy(w, reader)
		}
	}))
	defer server.Close()
	local.cfg.ArtifactHTTP = server.Client()
	remote.cfg.ArtifactHTTP = server.Client()
	repository := api.NodeBackupRepository{Location: filepath.Join(t.TempDir(), "repository"), Password: "recovery-transfer-test-password"}
	if _, err := local.ConnectBackupRepository(ctx, repository, true); err != nil {
		t.Fatal(err)
	}
	definition := json.RawMessage(`{"spec":{"networks":[],"assets":[]},"state":"captured"}`)
	plan := api.NodeBackupPlan{Repository: repository, RecoveryPointId: point, Definition: definition,
		Sources: []api.NodeRecoverySource{
			{EnvironmentId: env, NodeId: local.cfg.ID, Execution: assets[0]},
			{EnvironmentId: env, NodeId: remote.cfg.ID, Endpoint: server.URL, Execution: assets[1]},
		}}
	failed, err := local.BackupRecovery(ctx, backupID, plan)
	if err == nil || failed.ManifestSnapshotId != "" {
		t.Fatalf("truncated transfer produced complete backup: %+v %v", failed, err)
	}
	store := local.backupStore(repository)
	manifests, err := store.Snapshots(ctx, "netlab:backup:"+backupID+"/manifest")
	if err != nil || len(manifests) != 0 {
		t.Fatalf("manifest published before all assets completed: %+v %v", manifests, err)
	}
	truncate = false
	complete, err := local.BackupRecovery(ctx, backupID, plan)
	if err != nil || len(complete.Parts) != 2 || complete.Parts["local"].SnapshotId != failed.Parts["local"].SnapshotId {
		t.Fatalf("backup retry: %+v %v", complete, err)
	}
	var manifest bytes.Buffer
	if err := store.Dump(ctx, complete.ManifestSnapshotId, "/manifest.json", &manifest); err != nil {
		t.Fatal(err)
	}
	var portable struct {
		Definition json.RawMessage               `json:"definition"`
		Parts      map[string]api.NodeBackupPart `json:"parts"`
	}
	if err := json.Unmarshal(manifest.Bytes(), &portable); err != nil || !bytes.Equal(portable.Definition, definition) || len(portable.Parts) != 2 {
		t.Fatalf("portable manifest: %s %v", manifest.String(), err)
	}
	for i, node := range []*Engine{local, remote} {
		if err := os.RemoveAll(recoveryDirectory(node.cfg.DataDir, point, assets[i])); err != nil {
			t.Fatal(err)
		}
	}
	for i, asset := range assets {
		part := complete.Parts[asset.Asset.Id]
		source := api.NodeRecoverySource{NodeId: local.cfg.ID, Endpoint: server.URL, EnvironmentId: env, Execution: asset,
			Backup: &api.NodeBackupSource{Repository: repository, SnapshotId: part.SnapshotId, SizeBytes: part.SizeBytes}}
		node := local
		if i == 0 {
			// Restore through the other node after the original recovery files have gone.
			node = remote
		}
		reader, _, err := node.openRecoverySource(ctx, point, source)
		if err != nil {
			t.Fatal(err)
		}
		destination := t.TempDir()
		receiveErr := receiveDirectoryArtifact(reader, destination)
		if err = errors.Join(receiveErr, reader.Close()); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(destination, "data.bin"))
		if err != nil || !bytes.Equal(data, content) {
			t.Fatalf("backup data differs: %v", err)
		}
		if _, err = readRecoveryManifest(destination, env, point, asset); err != nil {
			t.Fatal(err)
		}
	}
	if err = local.DeleteBackup(ctx, backupID, repository); err != nil {
		t.Fatal(err)
	}
	remaining, err := store.Snapshots(ctx, "netlab:backup:"+backupID)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("backup parts remain after deletion: %+v %v", remaining, err)
	}
}
