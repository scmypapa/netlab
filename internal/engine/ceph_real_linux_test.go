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

func TestRealManagedCephLifecycle(t *testing.T) {
	device, address := os.Getenv("NETLAB_REAL_CEPH_DEVICE"), os.Getenv("NETLAB_REAL_CEPH_ADDRESS")
	if device == "" || address == "" {
		t.Skip("explicit isolated device and host address select destructive Ceph lifecycle validation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	data := t.TempDir()
	v, err := NewVirtualMachines("qemu:///system", data, "br-int")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	e := Engine{cfg: Config{DataDir: data, StorageDevice: device, AdvertiseAddress: address}, vm: v, locks: make(map[string]*objectLock)}
	id := uuid.NewString()
	t.Cleanup(func() {
		if err := e.RemoveCeph(context.Background(), id); err != nil {
			t.Error(err)
		}
	})
	started := time.Now()
	bootstrap, err := e.BootstrapCeph(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	host, err := e.JoinCeph(ctx, id, bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := e.ConfigureCeph(ctx, id, api.NodeCephConfiguration{Hosts: []api.NodeCephHost{host}})
	if err != nil {
		t.Fatal(err)
	}
	info, err := e.RegisterStorage(ctx, id, api.CreateStoragePool{NodeIds: []string{"test"}, Driver: api.StorageDriverRBD, Ceph: &connection})
	if err != nil || info.AvailableBytes <= 0 {
		t.Fatalf("pool registration: %v", err)
	}
	t.Logf("managed bootstrap and usable pool: %s", time.Since(started))
	if _, err = e.ConfigureCeph(ctx, id, api.NodeCephConfiguration{Hosts: []api.NodeCephHost{host}}); err != nil {
		t.Fatal(err)
	}
	base := vmDisk{root: info.Path, rbd: info.Rbd, image: info.Rbd.ImagePrefix + "test-base", snapshot: "base"}
	if err = base.prepare(ctx, "", 1); err != nil {
		t.Fatal(err)
	}
	if err = base.capture(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	child := vmDisk{root: info.Path, rbd: info.Rbd, image: info.Rbd.ImagePrefix + "test-clone"}
	if err = child.cloneSnapshot(ctx, base, 1); err != nil {
		t.Fatal(err)
	}
	if err = command(ctx, "qemu-io", "-f", "raw", "-c", "write -P 0xa5 0 1M", child.address()); err != nil {
		t.Fatal(err)
	}
	if err = child.remove(ctx); err != nil {
		t.Fatal(err)
	}
	if err = base.removeTemplateBase(ctx); err != nil {
		t.Fatal(err)
	}
	if err = e.RemoveStorage(ctx, id, ""); err != nil {
		t.Fatal(err)
	}
	if err = e.PrepareCephRemoval(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err = e.RemoveCeph(ctx, id); err != nil {
		t.Fatal(err)
	}
	out, err := commandOutput(ctx, "cephadm", "ls")
	if err != nil {
		t.Fatal(err)
	}
	var daemons []struct{ Fsid string }
	if err = json.Unmarshal(out, &daemons); err != nil {
		t.Fatal(err)
	}
	for _, daemon := range daemons {
		if daemon.Fsid == id {
			t.Fatal("cluster daemon was left behind")
		}
	}
	if _, err = os.Stat(filepath.Join(data, "ceph", id)); !os.IsNotExist(err) {
		t.Fatal("managed credentials were left behind")
	}
}
