//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"netlab.local/core/api"
)

func TestRealTemplateVolumeMetadataFromArchive(t *testing.T) {
	if os.Getenv("NETLAB_REAL_CONTAINER") == "" {
		t.Skip("set NETLAB_REAL_CONTAINER for the actual OCI archive and containerd")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	data, err := os.MkdirTemp("/tmp", "netlab-volume-metadata-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(data)
	archive := nonRootContainerArchive(t, ctx, data, "/work", "/cache")
	container, err := NewContainers(ctx, "/run/containerd/containerd.sock", data, uuid.NewString(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer container.Close()
	engine := &Engine{cfg: Config{ID: container.node, DataDir: data}, container: container, ioSlots: make(chan struct{}, 1)}
	template := api.Template{Id: uuid.NewString(), Kind: api.Container, Version: 1, Source: archive}
	defer func() {
		if err := engine.RemoveTemplate(context.Background(), template.Id, nil); err != nil {
			t.Error(err)
		}
	}()
	template, err = engine.PrepareTemplate(ctx, api.NodeTemplatePreparation{Template: template})
	if err != nil || template.ImageVolumes == nil || !slices.Equal(*template.ImageVolumes, []string{"/cache", "/work"}) {
		t.Fatalf("prepared image volume metadata: %+v, %v", template, err)
	}
	if _, err = os.Stat(filepath.Join(templateDirectory(data, template.Id, 1), "image.tar")); !os.IsNotExist(err) {
		t.Fatalf("prepared image retained a duplicate archive: %v", err)
	}
	source := api.StorageUsageSource{Execution: api.AssetExecution{InstanceId: uuid.NewString(), Asset: api.Asset{Id: uuid.NewString()}, Template: template}}
	usage, err := engine.StorageUsage(ctx, api.NodeStorageUsage{EnvironmentId: "storage", Sources: []api.StorageUsageSource{source}})
	if err != nil || len(usage) < 2 {
		t.Fatalf("image content and unpacked layers: %+v, %v", usage, err)
	}
	var imageBytes int64
	for _, part := range usage {
		if part.Bytes <= 0 || part.Bytes != part.BaseBytes || part.Storage == nil || part.Filesystem == "" {
			t.Fatalf("image storage classification: %+v", part)
		}
		imageBytes += part.Bytes
	}
	duplicate, err := engine.StorageUsage(ctx, api.NodeStorageUsage{EnvironmentId: "storage", Sources: []api.StorageUsageSource{source, source}})
	if err != nil {
		t.Fatal(err)
	}
	var duplicateBytes int64
	for _, part := range duplicate {
		duplicateBytes += part.Bytes
	}
	if imageBytes != duplicateBytes {
		t.Fatalf("shared container layers counted twice: %d != %d", imageBytes, duplicateBytes)
	}
	reader, _, err := engine.OpenTemplateArtifact(ctx, template.Id, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	legacy := t.TempDir()
	err = receiveDirectoryArtifact(reader, legacy)
	closeErr := reader.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	if err = os.RemoveAll(templateDirectory(data, template.Id, 1)); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(legacy, templateDirectory(data, template.Id, 1)); err != nil {
		t.Fatal(err)
	}
	// Import a portable archive produced by the same public transfer path.
	template.ImageVolumes = nil
	raw, err := json.Marshal(template)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(templateDirectory(data, template.Id, 1), "template.json"), raw, 0640); err != nil {
		t.Fatal(err)
	}
	reference := fmt.Sprintf("netlab/template/%s:1", template.Id)
	if err := container.client.ImageService().Delete(ctx, reference); err != nil {
		t.Fatal(err)
	}
	prepared, err := engine.PrepareTemplate(ctx, api.NodeTemplatePreparation{Template: template})
	if err != nil || prepared.ImageVolumes == nil || !slices.Equal(*prepared.ImageVolumes, []string{"/cache", "/work"}) {
		t.Fatalf("upgrade cached archive metadata: %+v, %v", prepared, err)
	}
	if _, err := container.client.GetImage(ctx, reference); err != nil {
		t.Fatalf("imported image is missing: %v", err)
	}
	if _, err = os.Stat(filepath.Join(templateDirectory(data, template.Id, 1), "image.tar")); !os.IsNotExist(err) {
		t.Fatalf("archive was retained after import: %v", err)
	}
	if prepared.ArtifactNodeId == nil || *prepared.ArtifactNodeId != engine.cfg.ID {
		t.Fatal("cached import changed the owning node")
	}
}
