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

	"github.com/containerd/containerd/errdefs"
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
	engine := &Engine{cfg: Config{ID: container.node, DataDir: data}, container: container, locks: map[string]*objectLock{}, ioSlots: make(chan struct{}, 1)}
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
	// Retain only the old exported template format, with no imported image left.
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
	if _, err := container.client.GetImage(ctx, reference); !errdefs.IsNotFound(err) {
		t.Fatalf("metadata upgrade imported the full image: %v", err)
	}
}
