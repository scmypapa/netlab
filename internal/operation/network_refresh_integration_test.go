package operation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"netlab.local/core/api"
	"netlab.local/core/db"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/transport"
)

func TestRoutingUpgradeReappliesNetworkWithoutTouchingGuests(t *testing.T) {
	pool := concurrencyPool(t, 4)
	ctx := context.Background()
	q := queries.New(pool)
	phases := []api.NodePlanPhase{}
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var plan api.NodePlan
		if err := json.NewDecoder(r.Body).Decode(&plan); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		phases = append(phases, plan.Phase)
		if calls.Add(1) == 1 {
			http.Error(w, "network temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		if plan.Phase != api.NodePlanPhaseNetwork || plan.Spec.Routers == nil || len(*plan.Spec.Routers) != 1 || plan.Spec.Networks[0].AccessAddress == nil || len(plan.Assets) != 1 || plan.Assets[0].InstanceId != "existing-instance" {
			t.Errorf("upgrade changed guest identity or used guest execution: %+v", plan)
		}
		json.NewEncoder(w).Encode(api.NodeResult{Results: []api.ExecutionResult{}})
	}))
	defer server.Close()
	node := "node"
	if err := q.PutNode(ctx, queries.PutNodeParams{ID: node, Name: "Node", Endpoint: server.URL, Info: []byte(`{"id":"node","capabilities":["network"],"capacity":{"cpu":4,"memoryMiB":4096,"diskGiB":100}}`)}); err != nil {
		t.Fatal(err)
	}
	template := api.Template{Id: "c", Kind: api.Container, Resources: api.Resources{Cpu: 1, MemoryMiB: 64, DiskGiB: 1}}
	definition, _ := json.Marshal(template)
	if err := q.CreateTemplate(ctx, queries.CreateTemplateParams{ID: template.Id, Definition: definition}); err != nil {
		t.Fatal(err)
	}
	spec := json.RawMessage(`{"networks":[{"id":"lan","name":"LAN","cidr":"192.0.2.0/24","gateway":"192.0.2.1"}],"assets":[{"id":"web","name":"Web","templateId":"c","resources":{"cpu":1,"memoryMiB":64,"diskGiB":1},"interfaces":[{"id":"nic","networkId":"lan","address":"192.0.2.2","mac":"02:00:00:00:00:02","primary":true}]}]}`)
	env, err := q.CreateEnvironment(ctx, queries.CreateEnvironmentParams{ID: uuid.NewString(), ProjectID: "default", Name: "Existing", Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	var environmentSpec api.EnvironmentSpec
	json.Unmarshal(spec, &environmentSpec)
	execution := api.AssetExecution{InstanceId: "existing-instance", Asset: environmentSpec.Assets[0], Template: template, Interfaces: []api.ResolvedInterface{{Id: "nic", NetworkId: "lan", PortName: "existing-port", Address: "192.0.2.2", Mac: "02:00:00:00:00:02"}}}
	raw, _ := json.Marshal(execution)
	if err := q.ReserveAsset(ctx, queries.ReserveAssetParams{EnvironmentID: env.ID, AssetID: "web", InstanceID: execution.InstanceId, NodeID: node, Execution: raw, Cpu: 1, MemoryMib: 64, DiskGib: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE runtime_assets SET current=true,state='running' WHERE environment_id=$1`, env.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET applied_spec=spec,status='running',network_node_id=$1,draft='{"spec":{"assets":[],"networks":[]},"view":{}}' WHERE id=$2`, node, env.ID); err != nil {
		t.Fatal(err)
	}
	beforeAssets, err := q.ListRuntimeAssets(ctx, env.ID)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := q.GetEnvironment(ctx, env.ID)
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE name='028_routing_domains.sql'`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.Migrate(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	ops, err := q.ListOperations(ctx, &env.ID)
	if err != nil || len(ops) != 1 || ops[0].Kind != "refresh-network" {
		t.Fatalf("migration should schedule one network refresh: %+v, %v", ops, err)
	}
	op := ops[0]
	owner := "test"
	claimed, err := q.ClaimOperation(ctx, queries.ClaimOperationParams{LeaseOwner: &owner})
	if err != nil || claimed.ID != op.ID {
		t.Fatalf("claim: %v", err)
	}
	w := Worker{Pool: pool, Queries: q, Client: &transport.Client{HTTP: server.Client()}}
	w.execute(ctx, queries.Operation(claimed))
	failed, err := q.GetOperation(ctx, op.ID)
	if err != nil || failed.State != "failed" {
		t.Fatalf("real failure was not recorded: %+v, %v", failed, err)
	}
	unchanged, err := q.GetEnvironment(ctx, env.ID)
	if err != nil || string(unchanged.AppliedSpec) != string(before.AppliedSpec) || unchanged.Revision != before.Revision {
		t.Fatalf("failed upgrade committed a new routing format: %v", err)
	}
	admin := access.Identity{Principal: queries.Principal{ID: "admin", Kind: "user", Administrator: true}}
	if _, err := (Service{Pool: pool, Queries: q}).Retry(ctx, admin, op.ID); err != nil {
		t.Fatal(err)
	}
	retry, err := q.ClaimOperation(ctx, queries.ClaimOperationParams{LeaseOwner: &owner})
	if err != nil || retry.ID != op.ID {
		t.Fatalf("retry claim: %v", err)
	}
	w.execute(ctx, queries.Operation(retry))
	result, _ := q.GetOperation(ctx, op.ID)
	if result.State != "succeeded" || !reflect.DeepEqual(phases, []api.NodePlanPhase{api.NodePlanPhaseNetwork, api.NodePlanPhaseNetwork}) {
		t.Fatalf("upgrade execution: %+v, phases %v", result, phases)
	}
	after, _ := q.GetEnvironment(ctx, env.ID)
	if after.Status != before.Status || !reflect.DeepEqual(after.Error, before.Error) || !reflect.DeepEqual(after.OperationID, before.OperationID) || after.Revision != before.Revision+1 || string(after.Draft) != string(before.Draft) || !strings.Contains(string(after.AppliedSpec), `"routers"`) || !strings.Contains(string(after.AppliedSpec), `"accessAddress"`) {
		t.Fatalf("upgrade did not preserve state and draft: %+v", after)
	}
	assets, _ := q.ListRuntimeAssets(ctx, env.ID)
	if len(assets) != 1 || assets[0].InstanceID != execution.InstanceId || !reflect.DeepEqual(assets, beforeAssets) {
		t.Fatal("network upgrade touched an existing guest execution")
	}
}
