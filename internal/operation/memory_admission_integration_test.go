package operation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/transport"
)

func TestHostMemoryAdmission(t *testing.T) {
	pool := concurrencyPool(t, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	q := queries.New(pool)
	storage := api.StorageInfo{Path: "/runtime", Filesystem: "disk", CapacityBytes: 100 << 30, AvailableBytes: 100 << 30}
	info := api.NodeInfo{Id: "node", Capabilities: []string{"container"}, Capacity: api.Resources{Cpu: 8, MemoryMiB: 4096, DiskGiB: 100}, Storage: &storage, ContainerStorage: &storage}
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(info) }))
	defer node.Close()
	raw, _ := json.Marshal(info)
	if err := q.PutNode(ctx, queries.PutNodeParams{ID: "node", Name: "Node", Endpoint: node.URL, Info: raw}); err != nil {
		t.Fatal(err)
	}
	ready := api.TemplateStateReady
	image := api.Template{Id: "image", Kind: api.Container, Version: 1, State: &ready}
	raw, _ = json.Marshal(image)
	if err := q.CreateTemplate(ctx, queries.CreateTemplateParams{ID: image.Id, Definition: raw}); err != nil {
		t.Fatal(err)
	}
	w := Worker{Pool: pool, Queries: q, Client: &transport.Client{HTTP: node.Client()}}
	create := func(t *testing.T, nodeID string, memory, previous int64) (queries.Environment, api.EnvironmentSpec) {
		t.Helper()
		asset := api.Asset{Id: "server", Name: "Server", TemplateId: image.Id, Resources: api.Resources{Cpu: 1, MemoryMiB: memory, DiskGiB: 1}, Interfaces: []api.Interface{}}
		spec := api.EnvironmentSpec{Assets: []api.Asset{asset}, Networks: []api.Network{}}
		raw, _ := json.Marshal(spec)
		env, err := q.CreateEnvironment(ctx, queries.CreateEnvironmentParams{ID: uuid.NewString(), ProjectID: "default", Name: "Memory", Spec: raw})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			for _, table := range []string{"events", "operations", "runtime_assets", "environments"} {
				column := "environment_id"
				if table == "environments" {
					column = "id"
				}
				if _, err := pool.Exec(ctx, "DELETE FROM "+table+" WHERE "+column+"=$1", env.ID); err != nil {
					t.Error(err)
				}
			}
		})
		if previous > 0 {
			asset.Resources.MemoryMiB = previous
			before := api.EnvironmentSpec{Assets: []api.Asset{asset}, Networks: []api.Network{}}
			raw, _ = json.Marshal(before)
			if _, err = pool.Exec(ctx, "UPDATE environments SET applied_spec=$2,status='running' WHERE id=$1", env.ID, raw); err != nil {
				t.Fatal(err)
			}
			execution := api.AssetExecution{InstanceId: uuid.NewString(), Template: image, Asset: asset, Interfaces: []api.ResolvedInterface{}}
			raw, _ = resourceRecords(env.ID, []Target{{NodeID: nodeID, Execution: execution}}, nil, false)
			if err = q.ReserveAssets(ctx, raw); err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(ctx, "UPDATE runtime_assets SET current=true,state='running' WHERE environment_id=$1", env.ID); err != nil {
				t.Fatal(err)
			}
		}
		return env, spec
	}
	operation := func(t *testing.T, env queries.Environment, kind string, assetID *string) queries.Operation {
		t.Helper()
		op, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: uuid.NewString(), EnvironmentID: &env.ID, AssetID: assetID, ScopeKind: "environment", ScopeID: env.ID, Kind: kind, Payload: []byte(`{}`), ExpectedRevision: env.Revision})
		if err != nil {
			t.Fatal(err)
		}
		owner := "memory-admission"
		op.LeaseOwner = &owner
		if _, err = pool.Exec(ctx, "UPDATE operations SET state='running',lease_owner=$2 WHERE id=$1", op.ID, owner); err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, "UPDATE environments SET operation_id=$2 WHERE id=$1", env.ID, op.ID); err != nil {
			t.Fatal(err)
		}
		return op
	}
	for _, test := range []struct {
		name                      string
		memory, previous, reserve int64
		accepted                  bool
		reserved                  int64
		stopped                   bool
	}{
		{"create-exact", 3072, 0, 0, true, 3072, false},
		{"create-above", 3073, 0, 0, false, 0, false},
		{"grow-exact", 3072, 2048, 0, true, 3072, false},
		{"grow-above", 3073, 2048, 0, false, 2048, false},
		{"shrink-after-budget-reduction", 2560, 3072, 2048, true, 3072, false},
		{"no-growth-after-budget-reduction", 3072, 3072, 2048, true, 3072, false},
		{"stopped-shrink", 2048, 3072, 0, true, 2048, true},
		{"stopped-grow-exact", 3072, 2048, 0, true, 3072, true},
		{"stopped-grow-above", 3073, 2048, 0, false, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := q.SetNodeScheduling(ctx, queries.SetNodeSchedulingParams{ID: "node", CpuAllocationRatio: 4, MemoryReserveMib: &test.reserve}); err != nil {
				t.Fatal(err)
			}
			env, spec := create(t, "node", test.memory, test.previous)
			kind := "start"
			if test.previous > 0 {
				kind = "change"
			}
			if test.stopped {
				kind = "start"
				if _, err := pool.Exec(ctx, "UPDATE runtime_assets SET state='stopped' WHERE environment_id=$1", env.ID); err != nil {
					t.Fatal(err)
				}
			}
			op := operation(t, env, kind, nil)
			payload := Payload{Spec: spec, BeforeStatus: "running"}
			err := w.plan(ctx, &op, &payload)
			if (err == nil) != test.accepted {
				t.Fatalf("accepted=%v, error=%v", test.accepted, err)
			}
			if err != nil && !strings.Contains(err.Error(), "容量不足") && !strings.Contains(err.Error(), "没有节点可承载资产") {
				t.Fatal(err)
			}
			reserved, err := q.GetReservedResources(ctx, "node")
			if err != nil || reserved.MemoryMib != test.reserved {
				t.Fatalf("reservation=%+v, error=%v", reserved, err)
			}
			if test.accepted && test.memory < test.previous {
				desired, _ := resourceRecords(env.ID, payload.Updates, nil, false)
				if err = q.UpdateExecutions(ctx, desired); err != nil {
					t.Fatal(err)
				}
				reserved, err = q.GetReservedResources(ctx, "node")
				if err != nil || reserved.MemoryMib != test.memory {
					t.Fatalf("applied shrink did not release memory: %+v, %v", reserved, err)
				}
			}
		})
	}

	t.Run("stopped instances release compute and concurrent starts reacquire it", func(t *testing.T) {
		reserve := int64(0)
		if _, err := q.SetNodeScheduling(ctx, queries.SetNodeSchedulingParams{ID: "node", CpuAllocationRatio: 4, MemoryReserveMib: &reserve}); err != nil {
			t.Fatal(err)
		}
		type candidate struct {
			env  queries.Environment
			spec api.EnvironmentSpec
			op   queries.Operation
		}
		candidates := make([]candidate, 2)
		nodeID := "node"
		for i := range candidates {
			env, spec := create(t, "node", 2048, 2048)
			rows, err := q.ListRuntimeAssets(ctx, env.ID)
			if err != nil || len(rows) != 1 {
				t.Fatalf("runtime assets: %v %v", rows, err)
			}
			raw, _ := json.Marshal([]api.ExecutionResult{{AssetId: "server", InstanceId: rows[0].InstanceID, NodeId: &nodeID, State: "stopped", ObservedAt: time.Now().UTC()}})
			if err = q.ApplyAssetResults(ctx, raw); err != nil {
				t.Fatal(err)
			}
			candidates[i] = candidate{env, spec, operation(t, env, "start", nil)}
		}
		assertReservation := func(cpu float64, memory, disk int64) {
			t.Helper()
			r, err := q.GetReservedResources(ctx, "node")
			if err != nil || r.Cpu != cpu || r.MemoryMib != memory || r.DiskGib != disk {
				t.Fatalf("reservation=%+v, error=%v", r, err)
			}
		}
		assertReservation(0, 0, 2)
		type outcome struct {
			op  queries.Operation
			err error
		}
		finished := make(chan outcome, len(candidates))
		for _, c := range candidates {
			go func() {
				p := Payload{Spec: c.spec, BeforeStatus: "stopped"}
				finished <- outcome{c.op, w.plan(ctx, &c.op, &p)}
			}()
		}
		var winner queries.Operation
		for range candidates {
			r := <-finished
			if r.err == nil {
				if winner.ID != "" {
					t.Fatal("concurrent starts overbooked node memory")
				}
				winner = r.op
			} else if !strings.Contains(r.err.Error(), "节点容量不足") {
				t.Fatal(r.err)
			}
		}
		if winner.ID == "" {
			t.Fatal("neither stopped instance could reacquire available compute")
		}
		// Preparation still reports a stopped process; its admitted start must retain capacity.
		assertReservation(1, 2048, 2)
		if _, err := pool.Exec(ctx, "UPDATE operations SET state='failed',phase='prepare' WHERE id=$1", winner.ID); err != nil {
			t.Fatal(err)
		}
		assertReservation(0, 0, 2)
		if _, err := pool.Exec(ctx, "UPDATE operations SET phase='rollback' WHERE id=$1", winner.ID); err != nil {
			t.Fatal(err)
		}
		assertReservation(1, 2048, 2)
		if _, err := pool.Exec(ctx, "UPDATE environments SET operation_id=NULL WHERE id=$1", winner.EnvironmentID); err != nil {
			t.Fatal(err)
		}
		assertReservation(0, 0, 2)
		if _, err := pool.Exec(ctx, "UPDATE environments SET operation_id=$2 WHERE id=$1", winner.EnvironmentID, winner.ID); err != nil {
			t.Fatal(err)
		}
		assertReservation(1, 2048, 2)
		if _, err := pool.Exec(ctx, "UPDATE operations SET phase='rolled-back' WHERE id=$1", winner.ID); err != nil {
			t.Fatal(err)
		}
		assertReservation(0, 0, 2)
	})

	t.Run("migration", func(t *testing.T) {
		source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(info) }))
		defer source.Close()
		raw, _ := json.Marshal(info)
		if err := q.PutNode(ctx, queries.PutNodeParams{ID: "source", Name: "Source", Endpoint: source.URL, Info: raw}); err != nil {
			t.Fatal(err)
		}
		reserve := int64(2048)
		if _, err := q.SetNodeScheduling(ctx, queries.SetNodeSchedulingParams{ID: "node", CpuAllocationRatio: 4, MemoryReserveMib: &reserve}); err != nil {
			t.Fatal(err)
		}
		create(t, "node", 1024, 1024)
		for _, test := range []struct {
			name   string
			memory int64
		}{{"exact", 1024}, {"above", 1025}} {
			t.Run(test.name, func(t *testing.T) {
				memory := test.memory
				env, spec := create(t, "source", memory, memory)
				assetID, target := "server", "node"
				op := operation(t, env, "migrate", &assetID)
				payload := Payload{Spec: spec, BeforeStatus: "running", Migration: &Migration{RequestedNode: &target}}
				err := w.planMigration(ctx, &op, &payload)
				if memory == 1024 {
					if err != nil || payload.Migration.Target == nil || payload.Migration.Target.NodeID != target {
						t.Fatalf("exact migration boundary rejected: %+v, %v", payload.Migration, err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "可用容量") {
					t.Fatalf("migration bypassed host reservation: %v", err)
				}
				reserved, err := q.GetReservedResources(ctx, target)
				want := int64(1024)
				if memory == 1024 {
					want += memory
				}
				if err != nil || reserved.MemoryMib != want {
					t.Fatalf("migration reservation=%+v, want=%d, error=%v", reserved, want, err)
				}
			})
		}
	})
}
