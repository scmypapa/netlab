//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd"
	filesystem "github.com/containerd/containerd/archive"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	"github.com/google/uuid"
	"libvirt.org/go/libvirt"
	"libvirt.org/go/libvirtxml"
)

func recoveryTestVM(t *testing.T) (context.Context, *libvirt.Connect, *libvirt.Domain, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	directory, err := os.MkdirTemp(os.TempDir(), "recovery-job-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	if err = os.Chmod(directory, 0777); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(directory, "source.qcow2")
	if err = command(ctx, "qemu-img", "create", "-f", "qcow2", disk, "64M"); err != nil {
		t.Fatal(err)
	}
	connection, err := libvirt.NewConnect("qemu:///system")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := connection.Close(); err != nil {
			t.Error(err)
		}
	})
	config := "<domain type='kvm'><name>netlab-recovery-job-" + uuid.NewString() + "</name><genid>" + uuid.NewString() + "</genid><memory unit='MiB'>128</memory><vcpu>1</vcpu><os><type arch='x86_64'>hvm</type></os><devices><disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='" + disk + "'/><target dev='vda' bus='virtio'/></disk></devices></domain>"
	domain, err := connection.DomainDefineXML(config)
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
		if err := domain.Free(); err != nil {
			t.Error(err)
		}
	})
	if err = domain.CreateWithFlags(libvirt.DOMAIN_START_PAUSED); err != nil {
		t.Fatal(err)
	}
	return ctx, connection, domain, directory
}

func TestRealRecoveryPreservesExternalBackup(t *testing.T) {
	if os.Getenv("NETLAB_REAL_BACKUP_JOB") == "" {
		t.Skip("set NETLAB_REAL_BACKUP_JOB to exercise native libvirt backup jobs")
	}
	ctx, _, domain, directory := recoveryTestVM(t)
	backup := libvirtxml.DomainBackup{Pull: &libvirtxml.DomainBackupPull{
		Server: &libvirtxml.DomainBackupPullServer{UNIX: &libvirtxml.DomainBackupPullServerUNIX{Socket: filepath.Join(directory, "backup.sock")}},
		Disks:  &libvirtxml.DomainBackupPullDisks{Disks: []libvirtxml.DomainBackupPullDisk{{Name: "vda", Backup: "yes", Scratch: &libvirtxml.DomainDiskSource{File: &libvirtxml.DomainDiskSourceFile{File: filepath.Join(directory, "scratch.qcow2")}}}}},
	}}
	external, err := backup.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err = domain.BackupBegin(external, "", 0); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := domain.AbortJob(); err != nil {
			t.Error(err)
		}
	}()
	before, err := domain.BackupGetXMLDesc(0)
	if err != nil {
		t.Fatal(err)
	}
	target := []libvirtxml.DomainBackupPushDisk{{Name: "vda", Backup: "yes", Target: &libvirtxml.DomainDiskSource{File: &libvirtxml.DomainDiskSourceFile{File: filepath.Join(directory, "capture.qcow2")}}}}
	if err = backupRecoveryDisks(ctx, domain, target, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "another native backup") {
		t.Fatalf("external backup was not rejected: %v", err)
	}
	after, err := domain.BackupGetXMLDesc(0)
	if err != nil || after != before {
		t.Fatalf("external backup was changed: %v", err)
	}
}

func TestRealRecoveryMemoryNative(t *testing.T) {
	if os.Getenv("NETLAB_REAL_MEMORY") == "" {
		t.Skip("set NETLAB_REAL_MEMORY to exercise native VM memory snapshots")
	}
	ctx, connection, domain, directory := recoveryTestVM(t)
	disk := filepath.Join(directory, "source.qcow2")
	initial, err := domain.GetID()
	if err != nil {
		t.Fatal(err)
	}
	memory := filepath.Join(directory, "memory.save")
	xml, err := domain.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		t.Fatal(err)
	}
	var definition libvirtxml.Domain
	if err = definition.Unmarshal(xml); err != nil {
		t.Fatal(err)
	}
	if err = captureRecoveryMemory(domain, memory, definition.Devices.Disks); err != nil {
		t.Fatal(err)
	}
	if state, err := vmState(domain); err != nil || state != "suspended" {
		t.Fatalf("capture resumed the guest: %s %v", state, err)
	}
	actual, err := domain.GetID()
	if err != nil || actual != initial {
		t.Fatalf("memory capture changed native instance: %d -> %d: %v", initial, actual, err)
	}
	xml, err = connection.DomainSaveImageGetXMLDesc(memory, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = domain.Destroy(); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(directory, "restored.qcow2")
	if err = copyArtifact(ctx, disk, target); err != nil {
		t.Fatal(err)
	}
	var restored libvirtxml.Domain
	if err = restored.Unmarshal(xml); err != nil {
		t.Fatal(err)
	}
	restored.Devices.Disks[0].Source.File.File = target
	generation := restored.GenID.Value
	xml, err = restored.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "domain.xml"), []byte(xml), 0600); err != nil {
		t.Fatal(err)
	}
	virtual := VirtualMachines{conn: connection}
	state, err := virtual.restoreRecoveryMemory(domain, directory)
	if err != nil || state != "suspended" {
		t.Fatalf("memory restore did not preserve paused state: %s %v", state, err)
	}
	xml, err = domain.GetXMLDesc(0)
	if err != nil {
		t.Fatal(err)
	}
	if err = restored.Unmarshal(xml); err != nil {
		t.Fatal(err)
	}
	if restored.GenID.Value == generation {
		t.Fatal("memory restore did not advance VM generation ID")
	}
	generation = restored.GenID.Value
	xml, err = domain.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		t.Fatal(err)
	}
	if err = restored.Unmarshal(xml); err != nil || restored.GenID.Value != generation {
		t.Fatalf("persistent generation differs from running guest: %v", err)
	}
	restoredDomain, err := connection.LookupDomainByName(restored.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer restoredDomain.Free()
	initial, err = restoredDomain.GetID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = virtual.restoreRecoveryMemory(restoredDomain, directory); err != nil {
		t.Fatal(err)
	}
	actual, err = restoredDomain.GetID()
	if err != nil || actual != initial {
		t.Fatalf("retry restarted restored memory: %d -> %d: %v", initial, actual, err)
	}
}

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
