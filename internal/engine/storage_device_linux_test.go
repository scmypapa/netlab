//go:build linux

package engine

import (
	"context"
	"netlab.local/core/api"
	"os"
	"testing"
)

func TestDiskAvailabilityProtectsExistingStorage(t *testing.T) {
	mount := "/data"
	for _, item := range []struct {
		name    string
		disk    blockDevice
		blocked bool
	}{
		{"empty", blockDevice{}, false},
		{"mounted", blockDevice{Mountpoints: []*string{&mount}}, true},
		{"filesystem", blockDevice{Filesystem: "ext4"}, true},
		{"partition", blockDevice{Children: []blockDevice{{Type: "part"}}}, true},
		{"readonly", blockDevice{ReadOnly: true}, true},
	} {
		t.Run(item.name, func(t *testing.T) {
			if (diskUnavailable(item.disk) != "") != item.blocked {
				t.Fatal("incorrect disk eligibility")
			}
		})
	}
}

func TestDiskSelectionSurvivesStartupConfiguration(t *testing.T) {
	e := Engine{cfg: Config{DataDir: t.TempDir(), StorageDevice: "/dev/initial"}}
	if err := e.initializeStorageSelection(); err != nil {
		t.Fatal(err)
	}
	if err := writeRecoveryJSON(e.storageDevicePath(), map[string]string{"device": "/dev/selected"}); err != nil {
		t.Fatal(err)
	}
	if err := e.initializeStorageSelection(); err != nil {
		t.Fatal(err)
	}
	selected, err := e.storageSelection()
	if err != nil || selected != "/dev/selected" {
		t.Fatal("startup flag overwrote managed selection", err)
	}
}

func TestDiskSelectionRejectsExistingFilesystems(t *testing.T) {
	e := Engine{cfg: Config{DataDir: t.TempDir()}, locks: map[string]*objectLock{}}
	inventory, err := e.StorageDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range inventory.Devices {
		if d.Available {
			continue
		}
		if _, err = e.ConfigureStorageDevice(context.Background(), api.ConfigureNodeStorage{Device: d.Path}); err == nil {
			t.Fatal("accepted an occupied disk")
		}
		if _, err = os.Stat(e.storageDevicePath()); !os.IsNotExist(err) {
			t.Fatal("invalid selection was persisted", err)
		}
		return
	}
	t.Skip("host has no occupied physical disk")
}
