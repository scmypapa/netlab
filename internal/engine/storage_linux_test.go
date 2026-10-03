//go:build linux

package engine

import (
	"os"
	"path/filepath"
	"testing"

	"netlab.local/core/api"
)

func TestDirectoryStorageOwnershipAndCleanup(t *testing.T) {
	source := t.TempDir()
	e := Engine{cfg: Config{ID: "node", DataDir: t.TempDir()}}
	info, err := e.RegisterStorage("pool", source)
	if err != nil {
		t.Fatal(err)
	}
	if info.Path != filepath.Join(source, "netlab-node", "pool") || info.CapacityBytes <= 0 || info.AvailableBytes <= 0 {
		t.Fatalf("invalid storage: %+v", info)
	}
	other, err := e.RegisterStorage("other", source)
	if err != nil || other.Filesystem != info.Filesystem {
		t.Fatalf("same filesystem not recognized: %+v %v", other, err)
	}
	a := api.AssetExecution{Asset: api.Asset{Id: "asset"}, InstanceId: "instance", StoragePath: &info.Path}
	instance := assetDirectory(e.cfg.DataDir, "environment", a)
	if err = os.MkdirAll(instance, 0711); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(instance, "data")
	if err = os.WriteFile(marker, []byte("persistent"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = e.RemoveStorage("pool", source); err == nil {
		t.Fatal("removed occupied storage")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "persistent" {
		t.Fatalf("persistent data lost: %s %v", data, err)
	}
	if err = os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err = e.RemoveStorage("pool", source); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(source); err != nil {
		t.Fatal("removed source directory:", err)
	}
	if _, err = os.Stat(other.Path); err != nil {
		t.Fatal("removed other pool:", err)
	}
}
