//go:build linux

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"netlab.local/core/api"
)

func TestTemplateTransferSharesContentAndReleasesLastReference(t *testing.T) {
	ctx := context.Background()
	e := &Engine{cfg: Config{DataDir: t.TempDir()}}
	const copies = 8
	paths := make([]string, copies)
	var wg sync.WaitGroup
	for i := range paths {
		wg.Go(func() {
			id := string(rune('a' + i))
			template := api.Template{Id: id, Version: 1, Kind: api.Vm, Disks: &[]api.TemplateDisk{{Id: "system"}}}
			source := t.TempDir()
			raw, err := json.Marshal(template)
			if err == nil {
				err = os.WriteFile(filepath.Join(source, "template.json"), raw, 0640)
			}
			if err == nil {
				err = os.WriteFile(systemDiskPath(source, 0), bytes.Repeat([]byte("immutable-disk"), 1<<16), 0640)
			}
			if err != nil {
				t.Error(err)
				return
			}
			reader, _, err := openDirectoryArtifact(source)
			if err != nil {
				t.Error(err)
				return
			}
			defer reader.Close()
			if err = e.installTemplateArtifact(ctx, template, reader, false); err != nil {
				t.Error(err)
				return
			}
			paths[i] = systemDiskPath(templateDirectory(e.cfg.DataDir, id, 1), 0)
		})
	}
	wg.Wait()
	first, err := os.Stat(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths[1:] {
		info, err := os.Stat(path)
		if err != nil || !os.SameFile(first, info) {
			t.Fatalf("identical transfers occupy separate files: %s %v", path, err)
		}
	}
	for i := range paths {
		if err = e.RemoveTemplate(ctx, string(rune('a'+i)), nil); err != nil {
			t.Fatal(err)
		}
		if i+1 < copies {
			content, err := os.ReadFile(paths[i+1])
			if err != nil || !bytes.Equal(content, bytes.Repeat([]byte("immutable-disk"), 1<<16)) {
				t.Fatalf("deleting a template damaged another: %v", err)
			}
		}
	}
	entries, err := os.ReadDir(filepath.Join(e.cfg.DataDir, "template-content"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("last deletion retained shared data: %d %v", len(entries), err)
	}
}

func TestArtifactTransferPreservesSparseFiles(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	data := make([]byte, 8<<20)
	copy(data[3<<20:], "a changed block between two holes")
	if err := os.WriteFile(filepath.Join(source, "disk.qcow2"), data, 0600); err != nil {
		t.Fatal(err)
	}
	reader, _, err := openDirectoryArtifact(source)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err = receiveDirectoryArtifact(reader, destination); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(destination, "disk.qcow2")
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatalf("transferred content differs: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if allocated := info.Sys().(*syscall.Stat_t).Blocks * 512; allocated >= int64(len(data))/2 {
		t.Fatalf("transfer allocated holes: %d bytes", allocated)
	}
}

func TestCompressedTemplateDisk(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root := t.TempDir()
	source, base, overlay := filepath.Join(root, "source.raw"), filepath.Join(root, "base.qcow2"), filepath.Join(root, "overlay.qcow2")
	if err := os.WriteFile(source, bytes.Repeat([]byte("netlab-storage\x00"), 1<<20), 0600); err != nil {
		t.Fatal(err)
	}
	if err := compressTemplateDisk(ctx, "raw", source, base); err != nil {
		t.Fatal(err)
	}
	if err := command(ctx, "qemu-img", "compare", "-f", "raw", "-F", "qcow2", source, base); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(base)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 1<<20 {
		t.Fatalf("compressible template grew to %d bytes", info.Size())
	}
	if err := command(ctx, "qemu-img", "create", "-f", "qcow2", "-F", "qcow2", "-b", base, overlay); err != nil {
		t.Fatal(err)
	}
	if err := command(ctx, "qemu-img", "compare", "-f", "raw", "-F", "qcow2", source, overlay); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryCompactionPreservesBaseAndChanges(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	data := t.TempDir()
	base := systemDiskPath(templateDirectory(data, "template", 1), 0)
	if err := os.MkdirAll(filepath.Dir(base), 0711); err != nil {
		t.Fatal(err)
	}
	original, capture := filepath.Join(data, "original.qcow2"), filepath.Join(data, "capture")
	if err := os.Mkdir(capture, 0711); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"qemu-img", "create", "-f", "qcow2", base, "64M"},
		{"qemu-io", "-f", "qcow2", "-c", "write -P 0x5a 0 32M", base},
		{"qemu-img", "create", "-f", "qcow2", "-F", "qcow2", "-b", base, original},
		{"qemu-io", "-f", "qcow2", "-c", "write -P 0x2a 8M 1M", original},
		{"qemu-io", "-f", "qcow2", "-c", "write -z 16M 1M", original},
		{"qemu-img", "convert", "-O", "qcow2", original, filepath.Join(capture, "disk.qcow2")},
	} {
		if err := command(ctx, args[0], args[1:]...); err != nil {
			t.Fatal(err)
		}
	}
	a := api.AssetExecution{Template: api.Template{Id: "template", Version: 1, Disks: &[]api.TemplateDisk{{Id: "boot"}}}}
	manifest := recoveryManifest{Disks: []recoveryDisk{{Serial: "image-boot", File: "disk.qcow2"}}}
	if err := compactRecoveryDisks(ctx, capture, a, &manifest, data); err != nil {
		t.Fatal(err)
	}
	if manifest.Disks[0].BackingTemplateDisk == nil {
		t.Fatal("recovery lost its shared base")
	}
	compact := filepath.Join(capture, "disk.qcow2")
	if err := command(ctx, "qemu-img", "compare", original, compact); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(compact)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() >= 2<<20 {
		t.Fatalf("recovery duplicated its base: %d", info.Size())
	}
}

func TestEnvironmentStorageCountsPhysicalBlocks(t *testing.T) {
	roots := []string{""}
	if root := os.Getenv("NETLAB_TEST_BTRFS"); root != "" {
		roots = append(roots, root)
	}
	for _, root := range roots {
		t.Run("storage"+root, func(t *testing.T) {
			data := t.TempDir()
			if root != "" {
				var err error
				data, err = os.MkdirTemp(root, "usage-")
				if err != nil {
					t.Fatal(err)
				}
				defer os.RemoveAll(data)
			}
			ctx := context.Background()
			e := Engine{cfg: Config{ID: "node", DataDir: data}}
			a := api.AssetExecution{InstanceId: "instance", Asset: api.Asset{Id: "asset"}, Template: api.Template{Id: "template", Version: 1, Kind: api.Vm, Disks: &[]api.TemplateDisk{{Id: "boot"}}}}
			instance := assetDirectory(data, "environment", a)
			checkpoint := recoveryDirectory(data, "point", a)
			base := templateDirectory(data, "template", 1)
			for _, directory := range []string{instance, checkpoint, base} {
				if err := os.MkdirAll(directory, 0711); err != nil {
					t.Fatal(err)
				}
			}
			disk := systemDiskPath(instance, 0)
			file, err := os.Create(disk)
			if err != nil {
				t.Fatal(err)
			}
			empty, err := e.StorageUsage(ctx, api.NodeStorageUsage{EnvironmentId: "environment", Sources: []api.StorageUsageSource{{Execution: a}}})
			if err != nil {
				t.Fatal(err)
			}
			for _, part := range empty {
				if part.Bytes != 0 || part.BaseBytes != 0 {
					t.Fatalf("empty disk occupies data blocks: %+v", part)
				}
			}
			if _, err = file.Write(bytes.Repeat([]byte("sparse-data"), 1<<16)); err != nil {
				t.Fatal(err)
			}
			if err = file.Truncate(1 << 40); err != nil {
				t.Fatal(err)
			}
			if err = file.Close(); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(systemDiskPath(base, 0), bytes.Repeat([]byte("base"), 1<<20), 0600); err != nil {
				t.Fatal(err)
			}
			if root != "" {
				err = cloneFile(disk, filepath.Join(checkpoint, "snapshot"))
			} else {
				err = os.Link(disk, filepath.Join(checkpoint, "snapshot"))
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Symlink(systemDiskPath(base, 0), filepath.Join(instance, "media.iso")); err != nil {
				t.Fatal(err)
			}
			shared := a
			shared.Template.Id = "shared-template"
			otherBase := templateDirectory(data, shared.Template.Id, 1)
			packets := filepath.Join(data, "packets", "environment")
			for _, directory := range []string{otherBase, packets} {
				if err = os.MkdirAll(directory, 0711); err != nil {
					t.Fatal(err)
				}
			}
			if err = os.Link(systemDiskPath(base, 0), systemDiskPath(otherBase, 0)); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(packets, "traffic.pcapng.zst"), bytes.Repeat([]byte("packet"), 1024), 0600); err != nil {
				t.Fatal(err)
			}
			if root != "" {
				directory, err := os.Open(data)
				if err != nil {
					t.Fatal(err)
				}
				err = unix.Syncfs(int(directory.Fd()))
				directory.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			usage, err := e.StorageUsage(ctx, api.NodeStorageUsage{EnvironmentId: "environment", Sources: []api.StorageUsageSource{{Execution: a}, {Execution: shared}, {Execution: a, RecoveryId: "point"}}})
			if err != nil {
				t.Fatal(err)
			}
			var allocated, baseBytes int64
			for _, part := range usage {
				allocated += part.Bytes
				baseBytes += part.BaseBytes
				if part.Storage == nil || part.Storage.CapacityBytes <= 0 || part.Filesystem == "" {
					t.Fatalf("missing physical capacity: %+v", part)
				}
			}
			if baseBytes <= 0 || allocated <= baseBytes || allocated-baseBytes > 1<<20 || allocated > 5<<20 {
				t.Fatalf("physical total must include one base and unique incremental blocks: total=%d base=%d", allocated, baseBytes)
			}
			if root == "" && baseBytes != 4<<20 {
				t.Fatalf("base image counted more than once: %d", baseBytes)
			}
		})
	}
}
