//go:build linux

package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/mount"
)

func TestRealDirectoryVolumeQuota(t *testing.T) {
	root := os.Getenv("NETLAB_REAL_VOLUME_ROOT")
	if root == "" {
		t.Skip("NETLAB_REAL_VOLUME_ROOT selects an isolated native test directory")
	}
	directory, err := os.MkdirTemp(root, "volume-quota-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	path := filepath.Join(directory, "data")
	defer removeDirectoryVolume(path)
	ctx := context.Background()
	if err = prepareDirectoryVolume(ctx, path, 1); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(path, "marker")
	if err = os.WriteFile(marker, []byte("durable"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = command(ctx, "fallocate", "-l", "1500M", filepath.Join(path, "over-limit")); err == nil {
		t.Fatal("write exceeded the one GiB filesystem")
	}
	if err = os.Remove(filepath.Join(path, "over-limit")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err = prepareDirectoryVolume(ctx, path, 2); err != nil {
		t.Fatal(err)
	}
	if err = command(ctx, "fallocate", "-l", "1500M", filepath.Join(path, "within-limit")); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(marker); err != nil || string(content) != "durable" {
		t.Fatalf("resize lost data: %q %v", content, err)
	}
	if err = prepareDirectoryVolume(ctx, path, 1); err == nil {
		t.Fatal("shrinking volume accepted")
	}
	current, err := mount.Lookup(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = command(ctx, "umount", path); err != nil {
		t.Fatal(err)
	}
	if err = prepareDirectoryVolume(ctx, path, 2); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(marker); err != nil || string(content) != "durable" {
		t.Fatalf("remount lost data: %q %v", content, err)
	}
	if err = removeDirectoryVolume(path); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path + ".ext4"); !os.IsNotExist(err) {
		t.Fatalf("volume image remains: %v", err)
	}
	if _, err = os.Stat(filepath.Join("/sys/class/block", filepath.Base(current.Source), "loop/backing_file")); err == nil {
		t.Fatal("volume loop device remains attached")
	}
}
