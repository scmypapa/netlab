//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"libvirt.org/go/libvirt"
	"libvirt.org/go/libvirtxml"
	"netlab.local/core/api"
	"netlab.local/core/internal/transport"
)

func TestRealNativeVMMigration(t *testing.T) {
	endpoint := os.Getenv("NETLAB_REAL_MIGRATION_ENDPOINT")
	if endpoint == "" {
		t.Skip("NETLAB_REAL_MIGRATION_ENDPOINT selects a second isolated worker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	client, err := transport.NewClient(os.Getenv("NETLAB_REAL_CA"), os.Getenv("NETLAB_REAL_CERT"), os.Getenv("NETLAB_REAL_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.MkdirTemp("/var/lib/netlab-dev", "native-migration-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(data) })
	if err = os.Chmod(data, 0711); err != nil {
		t.Fatal(err)
	}
	vm, err := NewVirtualMachines("qemu:///system", data, "br-int")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(vm.Close)
	e := Engine{cfg: Config{ID: uuid.NewString(), DataDir: data, LibvirtURI: "qemu:///system", ArtifactHTTP: client.HTTP}, vm: vm}
	pool, env := uuid.NewString(), uuid.NewString()
	raw, err := os.ReadFile(os.Getenv("NETLAB_REAL_CEPH_KEYRING"))
	if err != nil {
		t.Fatal(err)
	}
	_, key, found := strings.Cut(string(raw), "key = ")
	if !found {
		t.Fatal("missing client key in isolated Ceph keyring")
	}
	key = strings.TrimSpace(key)
	request := api.CreateStoragePool{Name: "Native migration", Driver: api.StorageDriverRBD, NodeIds: []string{e.cfg.ID}, Ceph: &api.CephConnection{Pool: "netlab", User: "netlab", Monitors: []string{"192.168.122.1:16789"}, Key: &key}}
	source, err := e.RegisterStorage(ctx, pool, request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.RemoveStorage(context.Background(), pool, ""); err != nil {
			t.Error(err)
		}
	})
	var target api.StorageInfo
	if err = client.Do(ctx, http.MethodPost, endpoint, "/node/v1/storage/"+pool, request, &target); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Do(context.Background(), http.MethodDelete, endpoint, "/node/v1/storage/"+pool, nil, nil); err != nil {
			t.Error(err)
		}
	})
	base := filepath.Join(data, "base.qcow2")
	if err = command(ctx, "qemu-img", "create", "-f", "qcow2", base, "1G"); err != nil {
		t.Fatal(err)
	}
	a := api.AssetExecution{InstanceId: uuid.NewString(), StoragePoolId: &pool, StoragePath: &source.Path, Rbd: source.Rbd,
		Asset:    api.Asset{Id: uuid.NewString(), Name: "Native migration verification", Resources: api.Resources{Cpu: 1, MemoryMiB: 128, DiskGiB: 1}, Interfaces: []api.Interface{}},
		Template: api.Template{Id: uuid.NewString(), Version: 1, Source: base, Kind: api.Vm, Format: ptr(api.Qcow2), Hardware: &api.Hardware{Machine: "q35", Firmware: api.Bios, DiskBus: api.HardwareDiskBusVirtio, NicModel: api.HardwareNicModelVirtio}}, Interfaces: []api.ResolvedInterface{}}
	a.Template, err = vm.prepareTemplate(ctx, a.Template, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = vm.prepare(ctx, env, a); err != nil {
		t.Fatal(err)
	}
	uri, close, err := e.migrationConnection(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(close)
	connection, err := libvirt.NewConnect(uri)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	t.Cleanup(func() {
		for _, c := range []*libvirt.Connect{vm.conn, connection} {
			domain, err := c.LookupDomainByUUIDString(a.InstanceId)
			if noDomain(err) {
				continue
			}
			if err != nil {
				t.Error(err)
				continue
			}
			active, err := domain.IsActive()
			if err == nil && active {
				err = domain.Destroy()
			}
			domain.Free()
			if err != nil {
				t.Error(err)
			}
		}
		remote := a
		remote.StoragePath, remote.Rbd = &target.Path, target.Rbd
		plan := api.NodePlan{OperationId: uuid.NewString(), EnvironmentId: env, Phase: api.NodePlanPhaseDestroy, Assets: []api.AssetExecution{remote}}
		var result api.NodeResult
		if err := client.Do(context.Background(), http.MethodPost, endpoint, "/node/v1/plans", plan, &result); err != nil {
			t.Error(err)
		}
		for _, asset := range result.Results {
			if asset.Error != nil {
				t.Error(*asset.Error)
			}
		}
		if _, err := vm.Execute(context.Background(), env, api.NodePlanPhaseDestroy, a); err != nil {
			t.Error(err)
		}
	})
	if _, err = vm.Execute(ctx, env, api.NodePlanPhaseStart, a); err != nil {
		t.Fatal(err)
	}
	domain, err := vm.conn.LookupDomainByUUIDString(a.InstanceId)
	if err != nil {
		t.Fatal(err)
	}
	text, err := domain.GetXMLDesc(libvirt.DOMAIN_XML_MIGRATABLE)
	domain.Free()
	if err != nil {
		t.Fatal(err)
	}
	var configuration libvirtxml.Domain
	if err = configuration.Unmarshal(text); err != nil {
		t.Fatal(err)
	}
	generation := configuration.GenID.Value
	destination := a
	destination.StoragePath, destination.Rbd = &target.Path, target.Rbd
	execution, err := json.Marshal(destination)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := xml.Marshal(Ownership{Environment: env, Asset: a.Asset.Id, Instance: a.InstanceId, Execution: string(execution)})
	if err != nil {
		t.Fatal(err)
	}
	configuration.Metadata.XML = string(metadata)
	configuration.SecLabel = nil
	configuration.Devices.Emulator = ""
	configuration.Memory.Value += 1024
	invalid, err := configuration.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.migrateVM(ctx, env, a, api.NodeVMMigration{Endpoint: endpoint, DomainXml: invalid}); err == nil {
		t.Fatal("native migration accepted an incompatible destination")
	}
	if state, err := vm.Execute(ctx, env, api.NodePlanPhaseInspect, a); err != nil || state != "running" {
		t.Fatalf("native rejection changed the source: %s %v", state, err)
	}
	if leftover, err := connection.LookupDomainByUUIDString(a.InstanceId); !noDomain(err) {
		if err == nil {
			leftover.Free()
		}
		t.Fatalf("rejected migration left a destination domain: %v", err)
	}
	configuration.Memory.Value -= 1024
	text, err = configuration.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	result, err := e.migrateVM(ctx, env, a, api.NodeVMMigration{Endpoint: endpoint, DomainXml: text})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "running" || result.InstanceId != a.InstanceId || result.Execution == nil || result.Execution.DataSetId != a.DataSetId {
		t.Fatalf("unexpected migration result: %+v", result)
	}
	domain, err = connection.LookupDomainByUUIDString(a.InstanceId)
	if err != nil {
		t.Fatal(err)
	}
	defer domain.Free()
	text, err = domain.GetXMLDesc(0)
	if err != nil {
		t.Fatal(err)
	}
	configuration = libvirtxml.Domain{}
	if err = configuration.Unmarshal(text); err != nil || configuration.GenID.Value != generation {
		t.Fatalf("migration changed VM generation identity: %v", err)
	}
	if _, err = e.migrateVM(ctx, env, a, api.NodeVMMigration{Endpoint: endpoint, DomainXml: text}); err != nil {
		t.Fatalf("repeated native migration did not observe its destination: %v", err)
	}
	t.Logf("native shared-RBD live migration and identity checks: %s", time.Since(start))
	if err = domain.Suspend(); err != nil {
		t.Fatal(err)
	}
	text, err = domain.GetXMLDesc(libvirt.DOMAIN_XML_MIGRATABLE)
	if err != nil {
		t.Fatal(err)
	}
	configuration = libvirtxml.Domain{}
	if err = configuration.Unmarshal(text); err != nil {
		t.Fatal(err)
	}
	sourceEndpoint := os.Getenv("NETLAB_REAL_MIGRATION_SOURCE")
	destination.StoragePath, destination.Rbd = &source.Path, source.Rbd
	execution, err = json.Marshal(destination)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err = xml.Marshal(Ownership{Environment: env, Asset: a.Asset.Id, Instance: a.InstanceId, Execution: string(execution)})
	if err != nil {
		t.Fatal(err)
	}
	configuration.Metadata.XML = string(metadata)
	configuration.Devices.Emulator = ""
	configuration.SecLabel = nil
	text, err = configuration.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	plan := api.NodePlan{OperationId: uuid.NewString(), EnvironmentId: env, Phase: api.NodePlanPhaseMigrate, Assets: []api.AssetExecution{a}, Migrations: map[string]api.NodeVMMigration{a.Asset.Id: {Endpoint: sourceEndpoint, DomainXml: text}}}
	var returned api.NodeResult
	start = time.Now()
	if err = client.Do(ctx, http.MethodPost, endpoint, "/node/v1/plans", plan, &returned); err != nil {
		t.Fatal(err)
	}
	if len(returned.Results) != 1 || returned.Results[0].Error != nil || returned.Results[0].State != "suspended" {
		raw, _ := json.Marshal(returned)
		t.Fatalf("paused VM migration failed: %s", raw)
	}
	if state, err := vm.Execute(ctx, env, api.NodePlanPhaseResume, a); err != nil || state != "running" {
		t.Fatalf("returned VM did not resume: %s %v", state, err)
	}
	t.Logf("paused native migration through node plan and resume: %s", time.Since(start))
}
