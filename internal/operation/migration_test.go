package operation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"netlab.local/core/api"
	"netlab.local/core/db"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/environment"
	"netlab.local/core/internal/transport"
)

func TestMigrationOwnershipHandoff(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		name := "lost-response"
		if rejected {
			name = "rejected-destination"
		}
		t.Run(name, func(t *testing.T) { testMigrationOwnershipHandoff(t, rejected) })
	}
}

func testMigrationOwnershipHandoff(t *testing.T, rejected bool) {
	dsn := os.Getenv("NETLAB_DATABASE_URL")
	if dsn == "" {
		t.Skip("NETLAB_DATABASE_URL selects PostgreSQL")
	}
	ctx := context.Background()
	base, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	schema := pgx.Identifier{"migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")}.Sanitize()
	if _, err = base.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer base.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	q := queries.New(pool)
	var transferred atomic.Bool
	var requests atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(api.NodeVMMigration{DomainXml: "native source XML"})
			return
		}
		if r.Method == http.MethodDelete {
			if !transferred.Load() {
				t.Error("source cleanup before transfer")
			}
			json.NewEncoder(w).Encode(api.NodeVMMigration{})
			return
		}
		var plan api.NodePlan
		json.NewDecoder(r.Body).Decode(&plan)
		a := plan.Assets[0]
		if plan.Phase != api.NodePlanPhaseMigrate {
			state := "running"
			var detail *string
			if transferred.Load() {
				state = "absent"
				message := "source has transferred the instance"
				detail = &message
			}
			json.NewEncoder(w).Encode(api.NodeResult{Results: []api.ExecutionResult{{AssetId: a.Asset.Id, InstanceId: a.InstanceId, State: state, Error: detail, ObservedAt: time.Now()}}})
			return
		}
		attempt := requests.Add(1)
		if rejected && attempt == 1 {
			message := "native migration rejected incompatible destination"
			json.NewEncoder(w).Encode(api.NodeResult{Results: []api.ExecutionResult{{AssetId: a.Asset.Id, InstanceId: a.InstanceId, State: "unknown", Error: &message, ObservedAt: time.Now()}}})
			return
		}
		transferred.Store(true)
		if !rejected && attempt == 1 {
			http.Error(w, "response lost after native transfer", http.StatusBadGateway)
			return
		}
		json.NewEncoder(w).Encode(api.NodeResult{Results: []api.ExecutionResult{{AssetId: a.Asset.Id, InstanceId: a.InstanceId, State: "running", ObservedAt: time.Now()}}})
	}))
	defer source.Close()
	rbd := api.RbdStorage{Fsid: "ceph", Pool: "shared", SecretId: "storage", ImagePrefix: "storage.", User: "netlab", Monitors: []string{"mon:123"}}
	storage := api.StorageInfo{Path: "/pool", Filesystem: "ceph/shared", Rbd: &rbd, CapacityBytes: 100 << 30, AvailableBytes: 90 << 30}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			if transferred.Load() {
				t.Error("rollback attempted after ownership transfer")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(storage)
			return
		}
		if r.URL.Path == "/node/v1/migrations/prepare" {
			json.NewEncoder(w).Encode(api.NodeVMMigration{DomainXml: "native target XML"})
			return
		}
		var plan api.NodePlan
		json.NewDecoder(r.Body).Decode(&plan)
		if !transferred.Load() {
			t.Error("target inspection before transfer")
		}
		a := plan.Assets[0]
		json.NewEncoder(w).Encode(api.NodeResult{Results: []api.ExecutionResult{{AssetId: a.Asset.Id, InstanceId: a.InstanceId, State: "running", Execution: &a, ObservedAt: time.Now()}}})
	}))
	defer target.Close()
	info := api.NodeInfo{
		Capacity: api.Resources{Cpu: 2, MemoryMiB: 1024, DiskGiB: 100}, Capabilities: []string{"vm"},
		VmHardware: &api.VmHardware{CpuModes: []string{"host-model"}, NicModels: []string{"virtio"}, Machines: []api.VmMachine{{Name: "q35", MaxVcpus: 8, Firmware: []string{"bios"}, DiskBuses: []string{"virtio"}}}},
	}
	raw, _ := json.Marshal(info)
	for id, endpoint := range map[string]string{"source": source.URL, "target": target.URL} {
		if err = q.PutNode(ctx, queries.PutNodeParams{ID: id, Name: id, Endpoint: endpoint, Info: raw}); err != nil {
			t.Fatal(err)
		}
	}
	if err = q.CreateStoragePool(ctx, queries.CreateStoragePoolParams{ID: "storage", NodeIds: []string{"source", "target"}, Name: "Shared", Driver: "rbd"}); err != nil {
		t.Fatal(err)
	}
	image := api.Template{Id: uuid.NewString(), Version: 1, Kind: api.Vm, Hardware: &api.Hardware{Machine: "q35", Firmware: api.Bios, DiskBus: api.HardwareDiskBusVirtio, NicModel: api.HardwareNicModelVirtio}}
	a := api.Asset{Id: uuid.NewString(), Name: "VM", TemplateId: image.Id, Resources: api.Resources{Cpu: 2, MemoryMiB: 1024, DiskGiB: 1}, Interfaces: []api.Interface{}}
	spec := api.EnvironmentSpec{Assets: []api.Asset{a}, Networks: []api.Network{}}
	raw, _ = json.Marshal(spec)
	env, err := q.CreateEnvironment(ctx, queries.CreateEnvironmentParams{ID: uuid.NewString(), ProjectID: "default", Name: "Migration", Spec: raw})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "UPDATE environments SET applied_spec=spec,status='running' WHERE id=$1", env.ID); err != nil {
		t.Fatal(err)
	}
	poolID := "storage"
	execution := api.AssetExecution{Asset: a, Template: image, InstanceId: uuid.NewString(), StoragePoolId: &poolID, StoragePath: &storage.Path, Rbd: &rbd, Interfaces: []api.ResolvedInterface{}}
	raw, _ = resourceRecords(env.ID, []Target{{NodeID: "source", Execution: execution}})
	if err = q.ReserveAssets(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "UPDATE runtime_assets SET current=true,state='running' WHERE environment_id=$1", env.ID); err != nil {
		t.Fatal(err)
	}
	identity := access.Identity{Principal: queries.Principal{Kind: "user", Administrator: true}}
	service := Service{Pool: pool, Queries: q}
	operator := access.Identity{Grants: []queries.Grant{{ScopeKind: "environment", ScopeID: env.ID, Permissions: []string{"operate"}}}}
	if _, err = service.Migrate(ctx, operator, env.ID, a.Id, api.MigrationRequest{}); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("operate permission allowed migration", err)
	}
	if _, err = service.Migrate(ctx, identity, env.ID, a.Id, api.MigrationRequest{ExpectedRevision: 1}); !errors.Is(err, environment.ErrConflict) {
		t.Fatal("stale revision allowed", err)
	}
	requestID := uuid.NewString()
	op, err := service.Migrate(ctx, identity, env.ID, a.Id, api.MigrationRequest{ClientRequestId: &requestID})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := service.Migrate(ctx, identity, env.ID, a.Id, api.MigrationRequest{ClientRequestId: &requestID})
	if err != nil || duplicate.Id != op.Id {
		t.Fatal("request identity changed", err)
	}
	owner := "test"
	worker := Worker{Pool: pool, Queries: q, Client: &transport.Client{HTTP: http.DefaultClient}}
	claimed, err := q.ClaimOperation(ctx, &owner)
	if err != nil {
		t.Fatal(err)
	}
	worker.execute(ctx, queries.Operation(claimed))
	failed, err := q.GetOperation(ctx, op.Id)
	phase := "migration-transfer"
	if rejected {
		phase = "rolled-back"
	}
	if err != nil || failed.State != "failed" || failed.Phase != phase {
		t.Fatalf("failure lost: %+v %v", failed, err)
	}
	for _, id := range []string{"source", "target"} {
		r, err := q.GetReservedResources(ctx, id)
		cpu, memory := int64(2), int64(1024)
		if rejected && id == "target" {
			cpu, memory = 0, 0
		}
		if err != nil || r.Cpu != cpu || r.MemoryMib != memory {
			t.Fatalf("reservation missing on %s: %+v %v", id, r, err)
		}
	}
	if !rejected {
		if _, err = (environment.Service{Pool: pool, Queries: q}).Action(ctx, identity, env.ID, a.Id, api.ActionRequest{Action: api.ActionRequestActionStart}); err == nil {
			t.Fatal("new action bypassed unfinished handoff")
		}
	} else {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = q.WithTx(tx).CreateOperation(ctx, queries.CreateOperationParams{ID: uuid.NewString(), EnvironmentID: &env.ID, ScopeKind: "environment", ScopeID: env.ID, Kind: "destroy", Payload: []byte(`{}`)})
		tx.Rollback(ctx)
		if err != nil {
			t.Fatal("rolled-back migration blocked environment destruction", err)
		}
	}
	if _, err = service.Retry(ctx, identity, op.Id); err != nil {
		t.Fatal(err)
	}
	claimed, err = q.ClaimOperation(ctx, &owner)
	if err != nil {
		t.Fatal(err)
	}
	worker.execute(ctx, queries.Operation(claimed))
	completed, err := q.GetOperation(ctx, op.Id)
	if err != nil || completed.State != "succeeded" {
		t.Fatalf("retry: %+v %v", completed, err)
	}
	actual, err := q.GetCurrentAsset(ctx, queries.GetCurrentAssetParams{EnvironmentID: env.ID, AssetID: a.Id})
	if err != nil || actual.NodeID != "target" || actual.InstanceID != execution.InstanceId {
		t.Fatal("ownership handoff failed", err)
	}
	for id, cpu := range map[string]int64{"source": 0, "target": 2} {
		r, err := q.GetReservedResources(ctx, id)
		if err != nil || r.Cpu != cpu {
			t.Fatal("capacity after commit", r, err)
		}
	}
	volumes, err := q.StorageReservations(ctx, []string{"source", "target"})
	if err != nil || len(volumes) != 1 || volumes[0].DiskGib != 1 {
		t.Fatal("shared disks counted twice", volumes, err)
	}
	row, err := q.GetEnvironment(ctx, env.ID)
	if err != nil || row.Revision != env.Revision || row.Status != "running" {
		t.Fatal("migration changed configuration revision or lost state", row, err)
	}
}
