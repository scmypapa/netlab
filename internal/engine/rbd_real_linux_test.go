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
	if used, err := target.usedBytes(ctx); err != nil || used != 0 {
		t.Fatalf("restore copied source data: %d %v", used, err)
	}
	if err = e.deleteRecovery(ctx, env, point, a); err != nil {
		t.Fatal(err)
	}
	if err = command(ctx, "qemu-io", "-f", "raw", "-c", "read -P 0x5a 0 1M", target.address()); err != nil {
		t.Fatal(err)
	}
}

func TestRealLocalToRBDRecoveryWithMedia(t *testing.T) {
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
	defer cancel()
	v, err := NewVirtualMachines("qemu:///system", t.TempDir(), "br-int")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	env, point, node := uuid.NewString(), uuid.NewString(), "test-node"
	disks := []api.TemplateDisk{{Id: "boot", SizeGiB: 1, Bus: api.Virtio, BootOrder: 1}}
	media := []api.TemplateMedia{{Id: "tools"}}
	a := api.AssetExecution{InstanceId: uuid.NewString(),
		Asset: api.Asset{Id: uuid.NewString(), Name: "Local to shared recovery", Resources: api.Resources{Cpu: 1, MemoryMiB: 128, DiskGiB: 1}},
		Template: api.Template{Id: uuid.NewString(), Version: 1, ArtifactNodeId: &node, Kind: api.Vm, Disks: &disks, Media: &media,
			Hardware: &api.Hardware{Machine: "q35", Firmware: api.Bios, DiskBus: api.HardwareDiskBusVirtio, NicModel: api.HardwareNicModelVirtio}}}
	templateDir := templateDirectory(v.data, a.Template.Id, 1)
	if err = os.MkdirAll(templateDir, 0711); err != nil {
		t.Fatal(err)
	}
	base := systemDiskPath(templateDir, 0)
	if err = command(ctx, "qemu-img", "create", "-f", "qcow2", base, "1G"); err != nil {
		t.Fatal(err)
	}
	if err = command(ctx, "qemu-io", "-f", "qcow2", "-c", "write -P 0x5a 0 1M", base); err != nil {
		t.Fatal(err)
	}
	if err = command(ctx, "genisoimage", "-quiet", "-o", templateMediaPath(templateDir, 0), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	localDir := assetDirectory(v.data, env, a)
	if err = os.MkdirAll(localDir, 0711); err != nil {
		t.Fatal(err)
	}
	if err = command(ctx, "qemu-img", "create", "-f", "qcow2", "-F", "qcow2", "-b", base, systemDiskPath(localDir, 0)); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(templateMediaPath(templateDir, 0), templateMediaPath(localDir, 0)); err != nil {
		t.Fatal(err)
	}
	text, err := DomainXML(env, localDir, "br-int", a)
	if err != nil {
		t.Fatal(err)
	}
	domain, err := v.conn.DomainDefineXML(text)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { domain.Undefine(); domain.Free() })
	e := Engine{cfg: Config{ID: node, DataDir: v.data}, vm: v, locks: make(map[string]*objectLock)}
	if _, err = e.captureRecovery(ctx, env, point, a, false, false); err != nil {
		t.Fatal(err)
	}
	manifest, err := readRecoveryManifest(recoveryDirectory(v.data, point, a), env, point, a)
	if err != nil || manifest.Disks[0].BackingTemplateDisk == nil {
		t.Fatalf("local backing capture: %v", err)
	}
	restored := a
	restored.InstanceId, restored.DataSetId = uuid.NewString(), uuid.NewString()
	restored.StoragePoolId, restored.StoragePath, restored.Rbd = ptr(filepath.Base(root)), &storage.Path, storage.Rbd
	t.Cleanup(func() {
		for _, err := range []error{e.removeRecoveryData(context.Background(), env, restored, false), e.deleteRecovery(context.Background(), env, point, a), os.RemoveAll(filepath.Join(root, "environments", env))} {
			if err != nil {
				t.Error(err)
			}
		}
	})
	sources := map[string]api.NodeRecoverySource{a.Asset.Id: {NodeId: node, EnvironmentId: env, Execution: a}}
	plan := api.NodePlan{EnvironmentId: env, RecoveryPointId: &point, RecoverySources: &sources}
	if err = e.prepareRecovery(ctx, plan, restored); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(templateMediaPath(assetDirectory(v.data, env, restored), 0)); err != nil {
		t.Fatalf("restored media is unavailable: %v", err)
	}
	if err = command(ctx, "qemu-io", "-f", "raw", "-c", "read -P 0x5a 0 1M", systemDisk(assetDirectory(v.data, env, restored), restored, 0).address()); err != nil {
		t.Fatal(err)
	}
}
