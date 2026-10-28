//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"libvirt.org/go/libvirt"
	"libvirt.org/go/libvirtxml"
	"netlab.local/core/api"
)

func executeVM(v *VirtualMachines, ctx context.Context, env string, phase api.NodePlanPhase, asset api.AssetExecution, releaseSlot func()) (string, error) {
	references, err := v.volumeReferences(ctx, env, phase, []api.AssetExecution{asset})
	if err != nil {
		return "unknown", err
	}
	return v.Execute(ctx, env, phase, asset, releaseSlot, references)
}

func TestRealVMVolumeReferenceLifecycle(t *testing.T) {
	if os.Getenv("NETLAB_REAL_VM_REFERENCES") != "1" {
		t.Skip("NETLAB_REAL_VM_REFERENCES=1 enables isolated libvirt domain and disk fixtures")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	connection, err := libvirt.NewConnect("qemu:///system")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	data := t.TempDir()
	v := &VirtualMachines{conn: connection, data: data}
	e := &Engine{vm: v, cfg: Config{DataDir: data}, locks: map[string]*objectLock{}, slots: make(chan struct{}, 4)}
	volumes := []api.Volume{{Id: "data", SizeGiB: 1}}
	asset := func() api.AssetExecution {
		return api.AssetExecution{InstanceId: uuid.NewString(), Template: api.Template{Kind: api.Vm}, Asset: api.Asset{Id: uuid.NewString(), Volumes: &volumes}}
	}
	disk := func(env string, a api.AssetExecution) string {
		return volumeDisk(assetDirectory(data, env, a), env, a, "data").file
	}
	define := func(env string, a api.AssetExecution) {
		t.Helper()
		path := disk(env, a)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			if err = command(ctx, "qemu-img", "create", "-f", "qcow2", path, "64M"); err != nil {
				t.Fatal(err)
			}
		}
		raw, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		metadata, err := xml.Marshal(Ownership{Environment: env, Asset: a.Asset.Id, Instance: a.InstanceId, Execution: string(raw)})
		if err != nil {
			t.Fatal(err)
		}
		config := libvirtxml.Domain{Type: "kvm", Name: "netlab-reference-" + a.InstanceId, UUID: a.InstanceId,
			Metadata: &libvirtxml.DomainMetadata{XML: string(metadata)}, Memory: &libvirtxml.DomainMemory{Value: 128, Unit: "MiB"}, VCPU: &libvirtxml.DomainVCPU{Value: 1},
			OS: &libvirtxml.DomainOS{Type: &libvirtxml.DomainOSType{Arch: "x86_64", Type: "hvm"}},
			Devices: &libvirtxml.DomainDeviceList{Disks: []libvirtxml.DomainDisk{{Device: "disk", Driver: &libvirtxml.DomainDiskDriver{Name: "qemu", Type: "qcow2"},
				Source: &libvirtxml.DomainDiskSource{File: &libvirtxml.DomainDiskSourceFile{File: path}}, Target: &libvirtxml.DomainDiskTarget{Dev: "vda", Bus: "virtio"}}}}}
		text, err := config.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		domain, err := connection.DomainDefineXML(text)
		if err != nil {
			t.Fatal(err)
		}
		domain.Free()
		t.Cleanup(func() {
			domain, err := connection.LookupDomainByUUIDString(a.InstanceId)
			if noDomain(err) {
				return
			}
			if err != nil {
				t.Error(err)
				return
			}
			defer domain.Free()
			if err = domain.Undefine(); err != nil {
				t.Error(err)
			}
		})
	}
	exists := func(env string, a api.AssetExecution, want bool) {
		t.Helper()
		_, err := os.Stat(disk(env, a))
		if want && err != nil || !want && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("volume exists=%v want=%v: %v", err == nil, want, err)
		}
	}
	execute := func(env string, assets ...api.AssetExecution) []api.ExecutionResult {
		t.Helper()
		result := e.Execute(ctx, api.NodePlan{EnvironmentId: env, Phase: api.NodePlanPhaseDestroy, Assets: assets})
		if result.Error != nil || len(result.Results) != len(assets) {
			t.Fatalf("destroy result=%+v", result)
		}
		return result.Results
	}
	env, foreignEnv := uuid.NewString(), uuid.NewString()
	old, foreign, failing := asset(), asset(), asset()
	replacement := old
	replacement.InstanceId = uuid.NewString()
	for _, a := range []api.AssetExecution{old, replacement, failing} {
		define(env, a)
	}
	define(foreignEnv, foreign)
	cleanup := e.Execute(ctx, api.NodePlan{EnvironmentId: env, Phase: api.NodePlanPhaseCleanupVolumes, Assets: []api.AssetExecution{old}})
	if cleanup.Error != nil || len(cleanup.Results) != 1 || cleanup.Results[0].Error == nil {
		t.Fatalf("attached volume cleanup was accepted: %+v", cleanup)
	}
	exists(env, old, true)
	if result := execute(env, old)[0]; result.Error != nil || result.State != "destroyed" {
		t.Fatalf("old instance destroy=%+v", result)
	}
	exists(env, replacement, true)
	exists(foreignEnv, foreign, true)
	if result := execute(env, old)[0]; result.Error != nil {
		t.Fatalf("absent instance retry=%+v", result)
	}
	exists(env, replacement, true)
	wrong := failing
	wrong.Asset.Id = uuid.NewString()
	results := execute(env, replacement, wrong)
	if results[0].Error != nil || results[0].State != "destroyed" || results[1].Error == nil {
		t.Fatalf("partial destroy=%+v", results)
	}
	exists(env, old, false)
	exists(env, failing, true)
	exists(foreignEnv, foreign, true)
	interrupted, stop := context.WithCancel(ctx)
	stop()
	if result := e.Execute(interrupted, api.NodePlan{EnvironmentId: env, Phase: api.NodePlanPhaseDestroy, Assets: []api.AssetExecution{failing}}); result.Error == nil {
		t.Fatal("cancelled destroy was reported as successful")
	}
	exists(env, failing, true)
	if result := execute(env, failing)[0]; result.Error != nil || result.State != "destroyed" {
		t.Fatalf("retry after failure=%+v", result)
	}
	exists(env, failing, false)
	exists(foreignEnv, foreign, true)
	missing := asset()
	define(env, missing)
	references, err := v.volumeReferences(ctx, env, api.NodePlanPhaseDestroy, []api.AssetExecution{missing})
	if err != nil {
		t.Fatal(err)
	}
	domain, err := connection.LookupDomainByUUIDString(missing.InstanceId)
	if err != nil {
		t.Fatal(err)
	}
	if err = domain.Undefine(); err != nil {
		t.Fatal(err)
	}
	domain.Free()
	if state, err := v.Execute(ctx, env, api.NodePlanPhaseDestroy, missing, nil, references); err != nil || state != "destroyed" {
		t.Fatalf("domain disappeared after snapshot: %s %v", state, err)
	}
	exists(env, missing, false)
	original := asset()
	original.DataSetId = "old"
	define(env, original)
	next := original
	next.DataSetId = "new"
	define(env, next)
	cleaned := e.Execute(ctx, api.NodePlan{EnvironmentId: env, Phase: api.NodePlanPhaseCleanupRecovery, Assets: []api.AssetExecution{original}, RecoveryTargets: map[string]api.AssetExecution{original.Asset.Id: next}})
	if cleaned.Error != nil || len(cleaned.Results) != 1 || cleaned.Results[0].Error != nil {
		t.Fatalf("old recovery dataset cleanup=%+v", cleaned)
	}
	exists(env, original, false)
	exists(env, next, true)
	if result := execute(env, next)[0]; result.Error != nil {
		t.Fatal(*result.Error)
	}
	if result := execute(foreignEnv, foreign)[0]; result.Error != nil {
		t.Fatal(*result.Error)
	}
}
