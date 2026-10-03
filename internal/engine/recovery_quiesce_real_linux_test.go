//go:build linux

package engine

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/google/uuid"
	"libvirt.org/go/libvirt"
	"netlab.local/core/api"
)

func TestRealRecoveryGuestFreezeOwnership(t *testing.T) {
	instance := os.Getenv("NETLAB_REAL_GUEST_RECOVERY")
	if instance == "" {
		t.Skip("set NETLAB_REAL_GUEST_RECOVERY to a running test VM with QEMU Guest Agent")
	}
	connection, err := libvirt.NewConnect("qemu:///system")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	domain, err := connection.LookupDomainByUUIDString(instance)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { domain.Free() })
	root, point := os.Getenv("NETLAB_REAL_GUEST_RECOVERY_ROOT"), os.Getenv("NETLAB_REAL_GUEST_RECOVERY_POINT")
	if root == "" {
		root = t.TempDir()
	}
	v := &VirtualMachines{conn: connection, data: root}
	owner, err := v.owned(domain, "", "")
	if err != nil {
		t.Fatal(err)
	}
	a := api.AssetExecution{InstanceId: instance, Asset: api.Asset{Id: owner.Asset}}
	if point != "" {
		frozen, err := v.freezeRecoveryFS(domain, point, a, false)
		if err != nil || !frozen {
			t.Fatalf("freeze: %v %v", frozen, err)
		}
		os.Exit(0) // Deliberately leave the native guest and ownership record for the next process.
	}
	point = uuid.NewString()
	t.Cleanup(func() {
		if err := v.finishRecoveryFS(owner.Environment, point, a, false); err != nil {
			t.Error(err)
		}
	})
	file, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(file, "-test.run=^TestRealRecoveryGuestFreezeOwnership$")
	child.Env = append(os.Environ(), "NETLAB_REAL_GUEST_RECOVERY_ROOT="+root, "NETLAB_REAL_GUEST_RECOVERY_POINT="+point)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("interrupted capture: %v %s", err, output)
	}
	if state, err := vmState(domain); err != nil || state != "suspended" {
		t.Fatalf("capture process did not leave VM suspended: %s %v", state, err)
	}
	if _, err := os.Stat(recoveryFreezePath(root, point, a)); err != nil {
		t.Fatal(err)
	}
	restarted := &VirtualMachines{conn: connection, data: root}
	if err := restarted.finishRecoveryFS(owner.Environment, point, a, false); err != nil {
		t.Fatal(err)
	}
	if state, err := guestFreezeState(domain); err != nil || state != "thawed" {
		t.Fatalf("guest remains frozen: %s %v", state, err)
	}
	if _, err := os.Stat(recoveryFreezePath(root, point, a)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("capture ownership was not removed: %v", err)
	}
	if err := domain.FSFreeze(nil, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := domain.FSThaw(nil, 0); err != nil {
			t.Error(err)
		}
	})
	if frozen, err := v.freezeRecoveryFS(domain, point, a, false); err == nil || frozen {
		t.Fatalf("external freeze was accepted as owned: %v %v", frozen, err)
	}
	if err := restarted.finishRecoveryFS(owner.Environment, point, a, false); err != nil {
		t.Fatal(err)
	}
	if state, err := guestFreezeState(domain); err != nil || state != "frozen" {
		t.Fatalf("external freeze was modified: %s %v", state, err)
	}
}
