//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"netlab.local/core/api"
)

func TestRealRBDRecovery(t *testing.T) {
	root := os.Getenv("NETLAB_REAL_RBD_ROOT")
	if root == "" {
		t.Skip("NETLAB_REAL_RBD_ROOT selects an isolated registered Ceph pool")
	}
	raw, err := os.ReadFile(filepath.Join(root, "storage.json"))
	if err != nil {
		t.Fatal(err)
	}
	var storage api.StorageInfo
	if err = json.Unmarshal(raw, &storage); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	v, err := NewVirtualMachines("qemu:///system", t.TempDir(), "br-int")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Close)
	disks := []api.TemplateDisk{{Id: "boot", SizeGiB: 1, Bus: api.Virtio, BootOrder: 1}}
	a := api.AssetExecution{InstanceId: uuid.NewString(), StoragePoolId: ptr(filepath.Base(root)), StoragePath: &storage.Path, Rbd: storage.Rbd,
		Asset:    api.Asset{Id: uuid.NewString(), Name: "Native RBD verification", Resources: api.Resources{Cpu: 1, MemoryMiB: 128, DiskGiB: 1}},
		Template: api.Template{Id: "native-rbd", Kind: api.Vm, Disks: &disks, Hardware: &api.Hardware{Machine: "q35", Firmware: api.Bios, DiskBus: api.HardwareDiskBusVirtio, NicModel: api.HardwareNicModelVirtio}}}
	env, point := uuid.NewString(), uuid.NewString()
	e := Engine{cfg: Config{DataDir: v.data}, vm: v}
	source := systemDisk(assetDirectory(v.data, env, a), a, 0)
	restored := a
	restored.DataSetId = uuid.NewString()
	target := systemDisk(assetDirectory(v.data, env, restored), restored, 0)
	t.Cleanup(func() {
		for _, err := range []error{e.deleteRecovery(ctx, env, point, a), source.remove(ctx), target.remove(ctx), os.RemoveAll(filepath.Join(root, "environments", env))} {
			if err != nil {
				t.Error(err)
			}
		}
	})
	if err = source.prepare(ctx, "", 1); err != nil {
		t.Fatal(err)
	}
	xml, err := DomainXML(env, assetDirectory(v.data, env, a), "br-int", a)
	if err != nil {
		t.Fatal(err)
	}
	domain, err := v.conn.DomainDefineXML(xml)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if active, err := domain.IsActive(); err != nil {
			t.Error(err)
		} else if active {
			if err = domain.Destroy(); err != nil {
				t.Error(err)
			}
		}
		if err := domain.Undefine(); err != nil {
			t.Error(err)
		}
		domain.Free()
	})
	if err = domain.Create(); err != nil {
		t.Fatal(err)
	}
	if actual, err := v.observedExecution(ctx, domain); err != nil || actual.Asset.Resources.DiskGiB != 1 {
		t.Fatalf("running RBD capacity: %+v %v", actual, err)
	}
	if err = domain.Destroy(); err != nil {
		t.Fatal(err)
	}
	if actual, err := v.observedExecution(ctx, domain); err != nil || actual.Asset.Resources.DiskGiB != 1 {
		t.Fatalf("stopped RBD capacity: %+v %v", actual, err)
	}
	if err = command(ctx, "qemu-io", "-f", "raw", "-c", "write -P 0x5a 0 1M", source.address()); err != nil {
		t.Fatal(err)
	}
	if _, err = e.captureRecovery(ctx, env, point, a, false, false); err != nil {
		t.Fatal(err)
	}
	if err = command(ctx, "qemu-io", "-f", "raw", "-c", "write -P 0xa5 0 1M", source.address()); err != nil {
		t.Fatal(err)
	}
	snapshot := source
	snapshot.snapshot = point
	if err = target.restoreSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if err = command(ctx, "qemu-io", "-f", "raw", "-c", "read -P 0x5a 0 1M", target.address()); err != nil {
		t.Fatal(err)
	}
	exported := t.TempDir()
	if err = e.copyRecovery(ctx, env, point, a, exported, ""); err != nil {
		t.Fatal(err)
	}
	if err = command(ctx, "qemu-io", "-f", "qcow2", "-c", "read -P 0x5a 0 1M", filepath.Join(exported, "disk-0.qcow2")); err != nil {
		t.Fatal(err)
	}
}
