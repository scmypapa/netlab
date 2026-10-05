//go:build linux

package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"libvirt.org/go/libvirt"
	"libvirt.org/go/libvirtxml"
	"netlab.local/core/api"
)

func TestRealNativeSnapshotAndNUMA(t *testing.T) {
	root := os.Getenv("NETLAB_REAL_SNAPSHOT_ROOT")
	if root == "" {
		t.Skip("NETLAB_REAL_SNAPSHOT_ROOT selects the isolated btrfs test mount")
	}
	info, err := StorageInfo(root)
	if err != nil || !info.NativeSnapshots {
		t.Fatalf("native snapshot storage: %+v %v", info, err)
	}
	data, err := os.MkdirTemp(root, "native-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(data); err != nil {
			t.Error(err)
		}
	})
	if err = os.Chmod(data, 0711); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	v, err := NewVirtualMachines("qemu:///system", data, "br-int")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Close)
	base := systemDiskPath(templateDirectory(data, "template", 1), 0)
	if err = os.MkdirAll(filepath.Dir(base), 0711); err != nil {
		t.Fatal(err)
	}
	if err = command(ctx, "qemu-img", "create", "-f", "qcow2", base, "1G"); err != nil {
		t.Fatal(err)
	}
	if err = command(ctx, "qemu-io", "-f", "qcow2", "-c", "write -P 0x5a 0 1M", base); err != nil {
		t.Fatal(err)
	}
	numa := 2
	placement := api.Strict
	disks := []api.TemplateDisk{{Id: "boot", SizeGiB: 1, Bus: api.Virtio, BootOrder: 1}}
	a := api.AssetExecution{InstanceId: uuid.NewString(), Asset: api.Asset{Id: uuid.NewString(), Name: "Native snapshot", Resources: api.Resources{Cpu: 4, MemoryMiB: 512, DiskGiB: 1}}, Template: api.Template{Id: "template", Version: 1, Kind: api.Vm, Disks: &disks, Hardware: &api.Hardware{Machine: "q35", Firmware: api.Bios, DiskBus: api.HardwareDiskBusVirtio, NicModel: api.HardwareNicModelVirtio, CpuTopology: &api.CpuTopology{Sockets: 1, Threads: 2}, NumaNodes: &numa}}}
	env, point := uuid.NewString(), uuid.NewString()
	a.Template.Hardware.NumaPlacement = &placement
	directory := assetDirectory(data, env, a)
	if err = os.MkdirAll(directory, 0711); err != nil {
		t.Fatal(err)
	}
	disk := systemDiskPath(directory, 0)
	if err = command(ctx, "qemu-img", "create", "-f", "qcow2", "-F", "qcow2", "-b", base, disk); err != nil {
		t.Fatal(err)
	}
	text, err := DomainXML(env, directory, "br-int", a)
	if err != nil {
		t.Fatal(err)
	}
	domain, err := v.conn.DomainDefineXML(text)
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
	actual, err := v.observedExecution(ctx, domain)
	if err != nil {
		t.Fatal(err)
	}
	if actual.Asset.Resources.Cpu != 4 || actual.Template.Hardware.CpuTopology.Threads != 2 || *actual.Template.Hardware.NumaNodes != 2 {
		t.Fatalf("actual hardware: %+v", actual)
	}
	if actual.Template.Hardware.NumaPlacement == nil || *actual.Template.Hardware.NumaPlacement != placement {
		t.Fatalf("host NUMA placement was lost: %+v", actual.Template.Hardware)
	}
	nodes, err := domain.GetNumaParameters(libvirt.DOMAIN_AFFECT_LIVE)
	if err != nil || !nodes.NodesetSet || nodes.Nodeset == "" {
		t.Fatalf("actual host memory binding: %+v %v", nodes, err)
	}
	if err = domain.Destroy(); err != nil {
		t.Fatal(err)
	}
	e := Engine{cfg: Config{DataDir: data}, vm: v}
	if _, err = e.captureRecovery(ctx, env, point, a, false, false); err != nil {
		t.Fatal(err)
	}
	recovery := recoveryDirectory(data, point, a)
	manifest, err := readRecoveryManifest(recovery, env, point, a)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Disks) != 1 || manifest.Disks[0].BackingTemplateDisk == nil || *manifest.Disks[0].BackingTemplateDisk != 0 {
		t.Fatalf("backing metadata: %+v", manifest.Disks)
	}
	if err = command(ctx, "qemu-io", "-f", "qcow2", "-c", "write -P 0xa5 0 1M", disk); err != nil {
		t.Fatal(err)
	}
	if err = command(ctx, "qemu-io", "-f", "qcow2", "-c", "read -P 0x5a 0 1M", filepath.Join(recovery, manifest.Disks[0].File)); err != nil {
		t.Fatal(err)
	}
	restored := a
	restored.InstanceId, restored.DataSetId = uuid.NewString(), uuid.NewString()
	staging := assetDirectory(data, env, restored)
	input := filepath.Join(staging, "input")
	if err = os.MkdirAll(input, 0711); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"domain.xml", manifest.Disks[0].File} {
		if err = copyArtifact(ctx, filepath.Join(recovery, file), filepath.Join(input, file)); err != nil {
			t.Fatal(err)
		}
	}
	if err = v.prepareRecovery(ctx, env, restored, input, staging, manifest); err != nil {
		t.Fatal(err)
	}
	if err = command(ctx, "qemu-io", "-f", "qcow2", "-c", "read -P 0x5a 0 1M", systemDiskPath(staging, 0)); err != nil {
		t.Fatal(err)
	}
	textBytes, err := os.ReadFile(filepath.Join(staging, "domain.xml"))
	if err != nil {
		t.Fatal(err)
	}
	var config libvirtxml.Domain
	if err = config.Unmarshal(string(textBytes)); err != nil || config.CPU.Numa == nil || len(config.CPU.Numa.Cell) != 2 {
		t.Fatalf("restored NUMA: %v", err)
	}
}
