//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/containerd/containerd"
	filesystem "github.com/containerd/containerd/archive"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	"github.com/google/uuid"
)

func TestRealRecoveryArchive(t *testing.T) {
	directory := os.Getenv("NETLAB_REAL_RECOVERY")
	if directory == "" {
		t.Skip("NETLAB_REAL_RECOVERY points to a captured container directory")
	}
	ctx := namespaces.WithNamespace(context.Background(), "netlab")
	raw, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest recoveryManifest
	if err = json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	client, err := containerd.New("/run/containerd/containerd.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	file, err := os.Open(filepath.Join(directory, "container.tar"))
	if err != nil {
		t.Fatal(err)
	}
	_, importErr := client.Import(ctx, file, containerd.WithSkipMissing())
	if err = errors.Join(importErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	ref := "netlab/recovery/" + manifest.PointID + "/" + manifest.Execution.Asset.Id
	defer func() {
		if err := client.ImageService().Delete(ctx, ref); err != nil {
			t.Error(err)
		}
	}()
	image, err := client.GetImage(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	container, err := client.Restore(ctx, uuid.NewString(), image, containerd.WithRestoreImage, containerd.WithRestoreRW)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
			t.Error(err)
		}
	}()
	info, err := container.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mounts, err := client.SnapshotService(info.Snapshotter).Mounts(ctx, info.SnapshotKey)
	if err != nil {
		t.Fatal(err)
	}
	if err = mount.WithTempMount(ctx, mounts, func(root string) error {
		marker, err := os.ReadFile(filepath.Join(root, "root/recovery-marker"))
		if err != nil {
			return err
		}
		if string(marker) != "original" {
			t.Fatalf("wrong writable layer marker: %q", marker)
		}
		if _, err = os.Stat(filepath.Join(root, "usr/share/nginx/html/50x.html")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("deleted base image file reappeared: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for index := range manifest.Volumes {
		destination := t.TempDir()
		file, err := os.Open(filepath.Join(directory, "volume-"+strconv.Itoa(index)+".tar"))
		if err != nil {
			t.Fatal(err)
		}
		_, applyErr := filesystem.Apply(ctx, destination, file)
		if err = errors.Join(applyErr, file.Close()); err != nil {
			t.Fatal(err)
		}
		marker, err := os.ReadFile(filepath.Join(destination, "marker"))
		if err != nil || string(marker) != "original" {
			t.Fatalf("volume marker: %q %v", marker, err)
		}
		link, err := os.Readlink(filepath.Join(destination, "marker-link"))
		if err != nil || link != "marker" {
			t.Fatalf("volume symlink: %s %v", link, err)
		}
		info, err := os.Stat(filepath.Join(destination, "marker"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("volume mode: %v %v", info, err)
		}
	}
}
