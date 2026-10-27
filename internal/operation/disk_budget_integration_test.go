package operation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/transport"
)

func TestContainerPlanUsesActualRootFilesystem(t *testing.T) {
	pool := concurrencyPool(t, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	q := queries.New(pool)
	info := api.NodeInfo{Id: "node", Capabilities: []string{"container"}, Capacity: api.Resources{Cpu: 8, MemoryMiB: 4096, DiskGiB: 1},
		Storage:          &api.StorageInfo{Path: "/metadata", Filesystem: "metadata", CapacityBytes: 1 << 30, AvailableBytes: 1 << 30},
		ContainerStorage: &api.StorageInfo{Path: "/containerd", Filesystem: "root", CapacityBytes: 10 << 30, AvailableBytes: 10 << 30}}
	data := api.StorageInfo{Path: "/data", Filesystem: "data", CapacityBytes: 100 << 30, AvailableBytes: 100 << 30}
	var failInfo atomic.Bool
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/node/v1/info" {
			if failInfo.Load() {
				http.Error(w, "runtime storage unavailable", http.StatusServiceUnavailable)
				return
			}
			json.NewEncoder(w).Encode(info)
		} else {
			json.NewEncoder(w).Encode(data)
		}
	}))
	defer node.Close()
	registered := info
	registered.ContainerStorage = &data
	raw, _ := json.Marshal(registered)
	if err := q.PutNode(ctx, queries.PutNodeParams{ID: "node", Name: "Node", Endpoint: node.URL, Info: raw}); err != nil {
		t.Fatal(err)
	}
	if err := q.CreateStoragePool(ctx, queries.CreateStoragePoolParams{ID: "data", NodeIds: []string{"node"}, Name: "Data", Driver: "directory", Directory: "/data"}); err != nil {
		t.Fatal(err)
	}
	ready := api.TemplateStateReady
	image := api.Template{Id: "image", Kind: api.Container, Version: 1, State: &ready}
	raw, _ = json.Marshal(image)
	if err := q.CreateTemplate(ctx, queries.CreateTemplateParams{ID: image.Id, Definition: raw}); err != nil {
		t.Fatal(err)
	}
	w := Worker{Pool: pool, Queries: q, Client: &transport.Client{HTTP: node.Client()}}
	plan := func(system, volume int64) (string, error) {
		id, dataPool, owner := uuid.NewString(), "data", "worker"
		asset := api.Asset{Id: "server", Name: "Server", TemplateId: image.Id, StoragePoolId: &dataPool, Resources: api.Resources{Cpu: 1, MemoryMiB: 128, DiskGiB: system}, Volumes: &[]api.Volume{{Id: "data", MountPath: "/data", SizeGiB: volume}}}
		spec := api.EnvironmentSpec{Assets: []api.Asset{asset}, Networks: []api.Network{}}
		raw, _ := json.Marshal(spec)
		env, err := q.CreateEnvironment(ctx, queries.CreateEnvironmentParams{ID: id, ProjectID: "default", Name: "Storage", Spec: raw})
		if err != nil {
			return id, err
		}
		op, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: uuid.NewString(), EnvironmentID: &id, ScopeKind: "environment", ScopeID: id, Kind: "start", Payload: []byte(`{}`), ExpectedRevision: env.Revision})
		if err != nil {
			return id, err
		}
		if _, err = pool.Exec(ctx, "UPDATE operations SET state='running',lease_owner=$2 WHERE id=$1", op.ID, owner); err != nil {
			return id, err
		}
		op.LeaseOwner = &owner
		return id, w.plan(ctx, &op, &Payload{Spec: spec})
	}
	id, err := plan(8, 3)
	if err != nil {
		t.Fatalf("data pool was incorrectly limited by the 1 GiB metadata filesystem: %v", err)
	}
	rows, err := q.StorageReservations(ctx, []string{"node"})
	if err != nil || len(rows) != 1 || rows[0].PoolID != "data" || rows[0].DiskGib != 3 || rows[0].ContainerGib != 8 {
		t.Fatalf("system and data budgets were charged to the wrong filesystem: %+v, %v", rows, err)
	}
	assets, err := q.ListRuntimeAssets(ctx, id)
	if err != nil || len(assets) != 1 || assets[0].SystemGib != 8 || assets[0].DiskGib != 11 {
		t.Fatalf("reservation components were not persisted atomically: %+v, %v", assets, err)
	}
	if _, err := plan(4, 3); err == nil {
		t.Fatal("100 GiB data pool masked exhaustion of the separate 10 GiB container filesystem")
	}
	failInfo.Store(true)
	if _, err := plan(2, 3); err == nil {
		t.Fatal("failed live runtime storage request used the obsolete registered filesystem")
	}
}

func TestRestoreDiskPeakKeepsExecutionAndBudgetSeparate(t *testing.T) {
	pool := concurrencyPool(t, 4)
	ctx := context.Background()
	q := queries.New(pool)
	if err := q.PutNode(ctx, queries.PutNodeParams{ID: "node", Name: "Node", Endpoint: "https://node.test", Info: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateEnvironment(ctx, queries.CreateEnvironmentParams{ID: "environment", ProjectID: "default", Name: "Restore", Spec: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	before := []Target{{NodeID: "node", Execution: api.AssetExecution{InstanceId: "instance", Template: api.Template{Kind: api.Container}, Asset: api.Asset{Id: "server", Resources: api.Resources{Cpu: 1, MemoryMiB: 128, DiskGiB: 2}, Volumes: &[]api.Volume{{Id: "data", SizeGiB: 4}}}}}}
	after := []Target{{NodeID: "node", Execution: api.AssetExecution{InstanceId: "instance", Template: api.Template{Kind: api.Container}, Asset: api.Asset{Id: "server", Resources: api.Resources{Cpu: 1, MemoryMiB: 128, DiskGiB: 3}, Volumes: &[]api.Volume{{Id: "data", SizeGiB: 6}}}}}}
	raw, _ := resourceRecords("environment", before, nil, false)
	if err := q.ReserveAssets(ctx, raw); err != nil {
		t.Fatal(err)
	}
	peak, _ := resourceRecords("environment", after, before, true)
	if err := q.ReserveResourceUpdates(ctx, peak); err != nil {
		t.Fatal(err)
	}
	if err := q.UpdateExecutions(ctx, peak); err != nil {
		t.Fatal(err)
	}
	assert := func(system, total int64) {
		t.Helper()
		rows, err := q.ListRuntimeAssets(ctx, "environment")
		if err != nil || len(rows) != 1 || rows[0].SystemGib != system || rows[0].DiskGib != total {
			t.Fatalf("storage peak was lost: %+v, %v", rows, err)
		}
		var execution api.AssetExecution
		if err := json.Unmarshal(rows[0].Execution, &execution); err != nil || execution.Asset.Resources.DiskGiB != 3 {
			t.Fatalf("guest configuration was falsified to store a reservation: %+v, %v", execution, err)
		}
	}
	assert(5, 15)
	desired, _ := resourceRecords("environment", after, nil, false)
	if err := q.UpdateExecutions(ctx, desired); err != nil {
		t.Fatal(err)
	}
	assert(3, 9)
	if _, err := pool.Exec(ctx, "UPDATE runtime_assets SET system_gib=disk_gib+1 WHERE instance_id='instance'"); err == nil {
		t.Fatal("database accepted an inconsistent system/total budget")
	}
}

func TestSameNodeMigrationReusesLocalVolumeBudget(t *testing.T) {
	pool := concurrencyPool(t, 4)
	ctx := context.Background()
	q := queries.New(pool)
	root := api.StorageInfo{Path: "/runtime", Filesystem: "root", CapacityBytes: 64 << 30, AvailableBytes: 60 << 30}
	data := api.StorageInfo{Path: "/data", Filesystem: "data", CapacityBytes: 20 << 30, AvailableBytes: 12 << 30}
	info := api.NodeInfo{Id: "node", Capabilities: []string{"container"}, Capacity: api.Resources{Cpu: 4, MemoryMiB: 4096, DiskGiB: 64}, Storage: &root, ContainerStorage: &root}
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/node/v1/info" {
			json.NewEncoder(w).Encode(info)
		} else {
			json.NewEncoder(w).Encode(data)
		}
	}))
	defer node.Close()
	raw, _ := json.Marshal(info)
	if err := q.PutNode(ctx, queries.PutNodeParams{ID: "node", Name: "Node", Endpoint: node.URL, Info: raw}); err != nil {
		t.Fatal(err)
	}
	if err := q.CreateStoragePool(ctx, queries.CreateStoragePoolParams{ID: "data", NodeIds: []string{"node"}, Name: "Data", Driver: "directory", Directory: data.Path}); err != nil {
		t.Fatal(err)
	}
	pv := "pv"
	if err := q.CreatePersistentVolume(ctx, queries.CreatePersistentVolumeParams{ID: pv, NodeID: "node", StoragePoolID: "data", Name: "Data", Kind: "container", SizeGib: 8}); err != nil {
		t.Fatal(err)
	}
	if err := q.FinishVolume(ctx, queries.FinishVolumeParams{ID: pv, State: "ready", SizeGib: 8}); err != nil {
		t.Fatal(err)
	}
	asset := api.Asset{Id: "server", Name: "Server", Resources: api.Resources{Cpu: 1, MemoryMiB: 128, DiskGiB: 2}, Volumes: &[]api.Volume{{Id: "data", MountPath: "/data", SizeGiB: 8, PersistentVolumeId: &pv}}}
	execution := api.AssetExecution{InstanceId: "source", Template: api.Template{Kind: api.Container}, Asset: asset, VolumeSources: map[string]api.NodeVolume{"data": {Id: pv, Kind: api.Container, SizeGiB: 8, Storage: data, StoragePoolId: "data"}}}
	spec := api.EnvironmentSpec{Assets: []api.Asset{asset}, Networks: []api.Network{}}
	raw, _ = json.Marshal(spec)
	env, err := q.CreateEnvironment(ctx, queries.CreateEnvironmentParams{ID: "env", ProjectID: "default", Name: "Migration", Spec: raw})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE environments SET applied_spec=spec,status='stopped' WHERE id=$1", env.ID); err != nil {
		t.Fatal(err)
	}
	raw, _ = resourceRecords(env.ID, []Target{{NodeID: "node", Execution: execution}}, nil, false)
	if err := q.ReserveAssets(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE runtime_assets SET current=true,state='stopped' WHERE environment_id=$1", env.ID); err != nil {
		t.Fatal(err)
	}
	targetPool, owner := "data", "worker"
	p := Payload{Spec: spec, BeforeStatus: "stopped", Migration: &Migration{RequestedPool: &targetPool}}
	raw, _ = json.Marshal(p)
	op, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: "migration", EnvironmentID: &env.ID, AssetID: &asset.Id, ScopeKind: "environment", ScopeID: env.ID, Kind: "migrate", ExpectedRevision: env.Revision, Payload: raw})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE operations SET state='running',lease_owner=$2 WHERE id=$1", op.ID, owner); err != nil {
		t.Fatal(err)
	}
	op.LeaseOwner = &owner
	w := Worker{Pool: pool, Queries: q, Client: &transport.Client{HTTP: node.Client()}}
	if err := w.planMigration(ctx, &op, &p); err != nil {
		t.Fatalf("same-node volume was charged as a new physical copy: %v", err)
	}
	if p.Migration.Target.Execution.VolumeSources["data"].StoragePoolId != "data" {
		t.Fatal("migration did not retain the actual volume pool")
	}
}
