//go:build linux

package operation

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"netlab.local/core/api"
	"netlab.local/core/db"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/transport"
)

func TestRealManagedStorageTwoNodes(t *testing.T) {
	endpoints := strings.Split(os.Getenv("NETLAB_REAL_STORAGE_NODES"), ",")
	if len(endpoints) != 2 || os.Getenv("NETLAB_DATABASE_URL") == "" {
		t.Skip("two isolated storage nodes and PostgreSQL select real cluster verification")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	base, err := pgxpool.New(ctx, os.Getenv("NETLAB_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	schema := pgx.Identifier{"storage_real_" + strings.ReplaceAll(uuid.NewString(), "-", "")}.Sanitize()
	if _, err = base.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer base.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	config, err := pgxpool.ParseConfig(os.Getenv("NETLAB_DATABASE_URL"))
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
	client, err := transport.NewClient(os.Getenv("NETLAB_NODE_CA"), os.Getenv("NETLAB_NODE_CERT"), os.Getenv("NETLAB_NODE_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	q := queries.New(pool)
	s := Service{Pool: pool, Queries: q}
	w := Worker{Pool: pool, Queries: q, Client: client}
	ids := []string{}
	for _, endpoint := range endpoints {
		info, err := client.Info(ctx, endpoint)
		if err != nil || info.StorageDevice == nil {
			t.Fatalf("storage node %s: %v", endpoint, err)
		}
		ids = append(ids, info.Id)
		raw, _ := json.Marshal(info)
		if err = q.PutNode(ctx, queries.PutNodeParams{ID: info.Id, Name: info.Name, Endpoint: endpoint, Info: raw}); err != nil {
			t.Fatal(err)
		}
		if err = s.ConfigureManagedStorage(ctx); err != nil {
			t.Fatal(err)
		}
	}
	owner := "real-storage-test"
	op, err := q.ClaimOperation(ctx, &owner)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	w.execute(ctx, queries.Operation(op))
	result, err := q.GetOperation(ctx, op.ID)
	if err != nil || result.State != "succeeded" {
		t.Fatalf("cluster configuration: %s %v %v", result.State, result.Error, err)
	}
	pools, err := q.ListStoragePools(ctx)
	if err != nil || len(pools) != 1 || pools[0].State != "ready" || len(pools[0].NodeIds) != 2 {
		t.Fatalf("published cluster: %v %v", pools, err)
	}
	storage := pools[0]
	t.Cleanup(func() {
		for _, endpoint := range []string{endpoints[1], endpoints[0]} {
			if err := client.Do(context.Background(), http.MethodDelete, endpoint, "/node/v1/ceph/"+storage.ID, nil, nil); err != nil {
				t.Error(err)
			}
		}
	})
	t.Logf("two storage nodes initialized: %s", time.Since(started))
	for _, endpoint := range endpoints {
		var info api.StorageInfo
		if err = client.Do(ctx, http.MethodGet, endpoint, transport.StorageRoute(storage.ID, ""), nil, &info); err != nil || info.AvailableBytes <= 0 {
			t.Fatalf("registered shared storage: %v", err)
		}
	}
	if err = s.ConfigureManagedStorage(ctx); err != nil {
		t.Fatal(err)
	}
	template := api.Template{Id: uuid.NewString(), Name: "Shared storage verification", Version: 1, Kind: api.Vm, Source: os.Getenv("NETLAB_REAL_STORAGE_TEMPLATE"),
		Hardware: &api.Hardware{Machine: "q35", Firmware: api.Bios, DiskBus: api.HardwareDiskBusVirtio, NicModel: api.HardwareNicModelVirtio}}
	if err = client.Do(ctx, http.MethodPost, endpoints[0], "/node/v1/templates/prepare", api.NodeTemplatePreparation{Template: template}, &template); err != nil {
		t.Fatal(err)
	}
	env := uuid.NewString()
	targets := []Target{}
	for i, endpoint := range endpoints {
		var info api.StorageInfo
		if err = client.Do(ctx, http.MethodGet, endpoint, transport.StorageRoute(storage.ID, ""), nil, &info); err != nil {
			t.Fatal(err)
		}
		a := api.AssetExecution{InstanceId: uuid.NewString(), Template: template, StoragePoolId: &storage.ID, StoragePath: &info.Path, Rbd: info.Rbd,
			Asset: api.Asset{Id: uuid.NewString(), Name: "Shared clone", Resources: api.Resources{Cpu: 1, MemoryMiB: 128, DiskGiB: 1}}}
		targets = append(targets, Target{NodeID: ids[i], Execution: a})
	}
	artifacts := map[string]string{ids[0]: endpoints[0], ids[1]: endpoints[1]}
	destroyed := make([]bool, len(targets))
	t.Cleanup(func() {
		for i, target := range targets {
			if destroyed[i] {
				continue
			}
			result, err := client.Execute(context.Background(), endpoints[i], api.NodePlan{EnvironmentId: env, OperationId: uuid.NewString(), Phase: api.NodePlanPhaseDestroy, Assets: []api.AssetExecution{target.Execution}})
			if err != nil || result.Error != nil || len(result.Results) != 1 || result.Results[0].Error != nil {
				t.Errorf("test VM cleanup: %+v %v", result, err)
			}
		}
	})
	if err = w.prepareSharedTemplates(ctx, targets, artifacts); err != nil {
		t.Fatal(err)
	}
	for i, target := range targets {
		for _, phase := range []api.NodePlanPhase{api.NodePlanPhasePrepare, api.NodePlanPhaseStart, api.NodePlanPhaseInspect, api.NodePlanPhaseForceStop, api.NodePlanPhaseDestroy} {
			result, err := client.Execute(ctx, endpoints[i], api.NodePlan{EnvironmentId: env, OperationId: uuid.NewString(), Phase: phase, Assets: []api.AssetExecution{target.Execution}, ArtifactEndpoints: &artifacts})
			if err != nil || result.Error != nil || len(result.Results) != 1 || result.Results[0].Error != nil {
				t.Fatalf("node %d phase %s: %+v %v", i, phase, result, err)
			}
			if phase == api.NodePlanPhaseInspect && result.Results[0].State != "running" {
				t.Fatalf("shared VM did not run: %+v", result.Results[0])
			}
			if phase == api.NodePlanPhaseDestroy {
				destroyed[i] = true
			}
		}
	}
	template.Version++
	if err = client.Do(ctx, http.MethodPost, endpoints[0], "/node/v1/templates/prepare", api.NodeTemplatePreparation{Template: template, StoragePoolId: &storage.ID}, &template); err != nil {
		t.Fatal(err)
	}
	templateDeleteID := uuid.NewString()
	templatePayload, _ := json.Marshal(Payload{Template: &template})
	if _, err = q.CreateOperation(ctx, queries.CreateOperationParams{ID: templateDeleteID, ScopeKind: "template", ScopeID: template.Id, Kind: "delete-template", Payload: templatePayload}); err != nil {
		t.Fatal(err)
	}
	op, err = q.ClaimOperation(ctx, &owner)
	if err != nil {
		t.Fatal(err)
	}
	w.execute(ctx, queries.Operation(op))
	result, err = q.GetOperation(ctx, templateDeleteID)
	if err != nil || result.State != "succeeded" {
		t.Fatalf("template deletion: %s %v %v", result.State, result.Error, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoints[0]+"/node/v1/templates/"+template.Id+"/versions/1", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.HTTP.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("old template version remains accessible: %d", response.StatusCode)
	}
	deleteID := uuid.NewString()
	if err = q.MarkStorageDeleting(ctx, queries.MarkStorageDeletingParams{ID: storage.ID, OperationID: &deleteID}); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(Payload{StoragePool: &api.CreateStoragePool{NodeIds: storage.NodeIds, Driver: api.StorageDriverRBD}})
	if _, err = q.CreateOperation(ctx, queries.CreateOperationParams{ID: deleteID, ScopeKind: "storage-pool", ScopeID: storage.ID, Kind: "delete-storage-pool", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	op, err = q.ClaimOperation(ctx, &owner)
	if err != nil {
		t.Fatal(err)
	}
	w.execute(ctx, queries.Operation(op))
	result, err = q.GetOperation(ctx, deleteID)
	if err != nil || result.State != "succeeded" {
		t.Fatalf("cluster deletion: %s %v %v", result.State, result.Error, err)
	}
	pools, err = q.ListStoragePools(ctx)
	if err != nil || len(pools) != 0 {
		t.Fatalf("cluster row was left behind: %v %v", pools, err)
	}
}
