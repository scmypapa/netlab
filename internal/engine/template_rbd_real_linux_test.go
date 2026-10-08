//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"netlab.local/core/api"
)

func TestRealSharedTemplateClones(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	v := VirtualMachines{data: t.TempDir()}
	disks := []api.TemplateDisk{{Id: "boot", SizeGiB: 1}}
	template := api.Template{Id: uuid.NewString(), Version: 1, Kind: api.Vm, Disks: &disks}
	source := systemDiskPath(templateDirectory(v.data, template.Id, 1), 0)
	if err = os.MkdirAll(filepath.Dir(source), 0711); err != nil {
		t.Fatal(err)
	}
	if err = command(ctx, "qemu-img", "create", "-f", "qcow2", source, "1G"); err != nil {
		t.Fatal(err)
	}
	if err = command(ctx, "qemu-io", "-f", "qcow2", "-c", "write -P 0x5a 0 32M", source); err != nil {
		t.Fatal(err)
	}
	base := sharedTemplateDisk(root, *storage.Rbd, template, 0)
	clones := make([]vmDisk, 8)
	for i := range clones {
		clones[i] = vmDisk{root: root, rbd: storage.Rbd, image: storage.Rbd.ImagePrefix + uuid.NewString()}
	}
	t.Cleanup(func() {
		for _, disk := range clones {
			if err := disk.remove(context.Background()); err != nil {
				t.Error(err)
			}
		}
		if err := base.removeTemplateBase(context.Background()); err != nil {
			t.Error(err)
		}
	})
	started := time.Now()
	if err = v.prepareSharedTemplate(ctx, template, storage); err != nil {
		t.Fatal(err)
	}
	t.Logf("base import: %s", time.Since(started))
	// Cached publication works with the source disk absent.
	if err = os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err = v.prepareSharedTemplate(ctx, template, storage); err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	var wg sync.WaitGroup
	for _, disk := range clones {
		wg.Go(func() {
			if err := disk.cloneSnapshot(ctx, base, 2); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	t.Logf("8 concurrent 2 GiB clones: %s", time.Since(started))
	var cloneBytes int64
	for _, disk := range clones {
		if err = command(ctx, "qemu-io", "-f", "raw", "-c", "read -P 0x5a 0 1M", disk.address()); err != nil {
			t.Fatal(err)
		}
		used, err := disk.usedBytes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		cloneBytes += used
	}
	if cloneBytes != 0 {
		t.Fatalf("clones copied %d bytes of base data", cloneBytes)
	}
	if err = command(ctx, "qemu-io", "-f", "raw", "-c", "write -P 0xa5 0 1M", clones[0].address()); err != nil {
		t.Fatal(err)
	}
	for _, disk := range append(clones[1:], base) {
		if err = command(ctx, "qemu-io", "-r", "-f", "raw", "-c", "read -P 0x5a 0 1M", disk.address()); err != nil {
			t.Fatal(err)
		}
	}
	baseBytes, err := base.usedBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	writtenBytes, err := clones[0].usedBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("base allocated=%d; 8 initial clones allocated=%d; modified clone allocated=%d", baseBytes, cloneBytes, writtenBytes)
	if err = base.removeTemplateBase(ctx); err == nil {
		t.Fatal("removed a referenced base")
	}
	if err = clones[7].remove(ctx); err != nil {
		t.Fatal(err)
	}
	if err = clones[7].cloneSnapshot(ctx, base, 2); err != nil {
		t.Fatalf("rejected deletion changed published base: %v", err)
	}
	template.Version++
	source = systemDiskPath(templateDirectory(v.data, template.Id, template.Version), 0)
	if err = os.MkdirAll(filepath.Dir(source), 0711); err != nil {
		t.Fatal(err)
	}
	if err = command(ctx, "qemu-img", "create", "-f", "qcow2", source, "1G"); err != nil {
		t.Fatal(err)
	}
	if err = v.prepareSharedTemplate(ctx, template, storage); err != nil {
		t.Fatal(err)
	}
	newBase := sharedTemplateDisk(root, *storage.Rbd, template, 0)
	poolID := filepath.Base(root)
	registration := filepath.Join(v.data, "storage-pools", poolID)
	if err = os.MkdirAll(registration, 0700); err != nil {
		t.Fatal(err)
	}
	if err = writeRecoveryJSON(filepath.Join(registration, "storage.json"), storage); err != nil {
		t.Fatal(err)
	}
	e := Engine{cfg: Config{DataDir: v.data}, locks: make(map[string]*objectLock)}
	if err = e.RemoveTemplate(ctx, template.Id, []string{poolID}); err == nil {
		t.Fatal("deleted a template with active version-one clones")
	}
	for _, disk := range clones {
		if err = disk.remove(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err = e.RemoveTemplate(ctx, template.Id, []string{poolID}); err != nil {
		t.Fatal(err)
	}
	for _, disk := range []vmDisk{base, newBase} {
		if exists, err := disk.exists(ctx); err != nil || exists {
			t.Fatalf("old or current shared template remains: %v", err)
		}
	}
	if _, err = os.Stat(filepath.Dir(filepath.Dir(source))); !os.IsNotExist(err) {
		t.Fatalf("template cache remains after deletion: %v", err)
	}
}
