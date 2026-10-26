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
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/transport"
)

func TestLegacyRoutingPayloadRespectsNetworkBoundary(t *testing.T) {
	for _, test := range []struct {
		kind, phase string
		replay      bool
	}{{"change", "queued", false}, {"change", "quiesce", false}, {"change", "rollback", false}, {"change", "verify", true}, {"change", "services", true}, {"vpn-create", "vpn", true}, {"vpn-revoke", "rollback", true}, {"move-network", "network-move-copy", false}, {"move-network", "network-move-services", true}, {"move-network", "network-move-vpn", true}} {
		t.Run(test.kind+"/"+test.phase, func(t *testing.T) {
			pool := concurrencyPool(t, 4)
			ctx := context.Background()
			q := queries.New(pool)
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var plan api.NodePlan
				if err := json.NewDecoder(r.Body).Decode(&plan); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				calls.Add(1)
				if plan.Phase != api.NodePlanPhaseNetwork || len(plan.Assets) != 1 || plan.Assets[0].InstanceId != "original" || plan.Spec.Networks[0].AccessAddress == nil {
					t.Errorf("replay touched a guest or lost its identity: %+v", plan)
				}
				json.NewEncoder(w).Encode(api.NodeResult{Results: []api.ExecutionResult{}})
			}))
			defer server.Close()
			if err := q.PutNode(ctx, queries.PutNodeParams{ID: "node", Name: "Node", Endpoint: server.URL, Info: []byte(`{}`)}); err != nil {
				t.Fatal(err)
			}
			asset := api.Asset{Id: "web", TemplateId: "c", Resources: api.Resources{Cpu: 0.25, MemoryMiB: 64, DiskGiB: 1}, Interfaces: []api.Interface{{Id: "nic", NetworkId: "lan", Address: "192.0.2.2", Mac: "02:00:00:00:00:02"}}}
			spec := api.EnvironmentSpec{Networks: []api.Network{{Id: "lan", Name: "LAN", Cidr: "192.0.2.0/24"}}, Assets: []api.Asset{asset}}
			raw, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			env, err := q.CreateEnvironment(ctx, queries.CreateEnvironmentParams{ID: uuid.NewString(), ProjectID: "default", Name: "Legacy", Spec: raw})
			if err != nil {
				t.Fatal(err)
			}
			execution := api.AssetExecution{InstanceId: "original", Asset: asset, Interfaces: []api.ResolvedInterface{{Id: "nic", NetworkId: "lan", Address: "192.0.2.2", PortName: "original-port"}}}
			encoded, _ := json.Marshal(execution)
			if err := q.ReserveAsset(ctx, queries.ReserveAssetParams{EnvironmentID: env.ID, AssetID: asset.Id, InstanceID: execution.InstanceId, NodeID: "node", Execution: encoded, Cpu: 0.25, MemoryMib: 64, DiskGib: 1}); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE runtime_assets SET current=true,state='running' WHERE environment_id=$1`, env.ID); err != nil {
				t.Fatal(err)
			}
			p := Payload{Spec: spec, BeforeSpec: &spec, Owner: &Target{NodeID: "node"}, Bindings: []api.NodeServiceBinding{{Id: "service", AssetId: "web", InterfaceId: "nic"}}}
			if strings.HasPrefix(test.kind, "vpn-") {
				p.Owner, p.Bindings = nil, nil
				if _, err := pool.Exec(ctx, `UPDATE environments SET applied_spec=spec,network_node_id='node' WHERE id=$1`, env.ID); err != nil {
					t.Fatal(err)
				}
			}
			if test.kind == "change" && test.phase == "rollback" {
				p.Spec.Networks = append([]api.Network(nil), spec.Networks...)
				gateway := "192.0.2.1"
				p.Spec.Networks[0].Cidr, p.Spec.Networks[0].Gateway = "192.0.2.0/30", &gateway
			}
			encoded, _ = json.Marshal(p)
			op, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: uuid.NewString(), EnvironmentID: &env.ID, ScopeKind: "environment", ScopeID: env.ID, Kind: test.kind, Payload: encoded, ExpectedRevision: 1})
			if err != nil {
				t.Fatal(err)
			}
			owner := "test"
			claimed, err := q.ClaimOperation(ctx, queries.ClaimOperationParams{LeaseOwner: &owner})
			if err != nil || claimed.ID != op.ID {
				t.Fatalf("claim failed: %v", err)
			}
			op = queries.Operation(claimed)
			worker := Worker{Pool: pool, Queries: q, Client: &transport.Client{HTTP: server.Client()}}
			if err := worker.phase(ctx, &op, &p, test.phase); err != nil {
				t.Fatal(err)
			}
			if err := worker.upgradeRouting(ctx, &op, &p); err != nil {
				t.Fatal(err)
			}
			want := int64(0)
			if test.replay {
				want = 1
			}
			canonical := p.Spec
			if test.kind == "change" && test.phase == "rollback" {
				canonical = *p.BeforeSpec
			}
			if calls.Load() != want || op.Phase != test.phase || canonical.Routers == nil || canonical.Networks[0].AccessAddress == nil || len(p.Bindings) > 0 && p.Bindings[0].NetworkId != "lan" || !reflect.DeepEqual(p.Spec.Assets, spec.Assets) {
				t.Fatalf("phase or immutable guest design changed: %d calls, %+v", calls.Load(), p)
			}
			if err := worker.upgradeRouting(ctx, &op, &p); err != nil || calls.Load() != want {
				t.Fatalf("canonical task replayed its upgrade again: %v", err)
			}
			assets, err := q.ListRuntimeAssets(ctx, env.ID)
			if err != nil || len(assets) != 1 || assets[0].InstanceID != execution.InstanceId || assets[0].State != "running" {
				t.Fatalf("upgrade changed the existing execution: %+v, %v", assets, err)
			}
		})
	}
}

func TestLegacyRoutingResumePersistsActualNetwork(t *testing.T) {
	for _, test := range []struct {
		name, kind, phase string
		committed, reject bool
	}{
		{"committed cleanup", "change", "cleanup", true, false},
		{"unchanged start", "start", "verify", false, false},
		{"replay failure rolls back", "change", "verify", false, true},
		{"rolled back terminal", "change", "rolled-back", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			pool := concurrencyPool(t, 4)
			ctx := context.Background()
			q := queries.New(pool)
			var networks atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var plan api.NodePlan
				if err := json.NewDecoder(r.Body).Decode(&plan); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				if plan.Phase == api.NodePlanPhaseNetwork && networks.Add(1) == 1 && test.reject {
					http.Error(w, "network replay rejected", http.StatusServiceUnavailable)
					return
				}
				results := []api.ExecutionResult{}
				for _, asset := range plan.Assets {
					if plan.Phase != api.NodePlanPhaseNetwork && plan.Phase != api.NodePlanPhaseInspect && plan.Phase != api.NodePlanPhaseStart && plan.Phase != api.NodePlanPhasePolicies {
						t.Errorf("upgrade unexpectedly changed guest lifecycle: %s", plan.Phase)
					}
					results = append(results, api.ExecutionResult{AssetId: asset.Asset.Id, InstanceId: asset.InstanceId, State: "running"})
				}
				json.NewEncoder(w).Encode(api.NodeResult{Results: results})
			}))
			defer server.Close()
			if err := q.PutNode(ctx, queries.PutNodeParams{ID: "node", Name: "Node", Endpoint: server.URL, Info: []byte(`{}`)}); err != nil {
				t.Fatal(err)
			}
			asset := api.Asset{Id: "web", TemplateId: "c", Resources: api.Resources{Cpu: 0.25, MemoryMiB: 64, DiskGiB: 1}, Interfaces: []api.Interface{{Id: "nic", NetworkId: "lan", Address: "192.0.2.2", Mac: "02:00:00:00:00:02"}}}
			spec := api.EnvironmentSpec{Networks: []api.Network{{Id: "lan", Name: "LAN", Cidr: "192.0.2.0/24"}}, Assets: []api.Asset{asset}}
			raw, _ := json.Marshal(spec)
			env, err := q.CreateEnvironment(ctx, queries.CreateEnvironmentParams{ID: "env", ProjectID: "default", Name: "Legacy", Spec: raw})
			if err != nil {
				t.Fatal(err)
			}
			execution := api.AssetExecution{InstanceId: "original", Asset: asset, Interfaces: []api.ResolvedInterface{{Id: "nic", NetworkId: "lan", Address: "192.0.2.2", PortName: "original-port"}}}
			encoded, _ := json.Marshal(execution)
			if err := q.ReserveAsset(ctx, queries.ReserveAssetParams{EnvironmentID: env.ID, AssetID: asset.Id, InstanceID: execution.InstanceId, NodeID: "node", Execution: encoded, Cpu: 0.25, MemoryMib: 64, DiskGib: 1}); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE runtime_assets SET current=true,state='running' WHERE environment_id='env'; UPDATE environments SET applied_spec=spec,status='running',network_node_id='node',draft='{}' WHERE id='env'`); err != nil {
				t.Fatal(err)
			}
			target := Target{NodeID: "node", Execution: execution, State: "running"}
			p := Payload{Spec: spec, BeforeSpec: &spec, BeforeStatus: "running", Owner: &Target{NodeID: "node"}, Before: []Target{target}, Unchanged: []Target{target}, Committed: test.committed}
			if test.reject || test.phase == "rolled-back" {
				p.Spec.Networks = append([]api.Network(nil), spec.Networks...)
				mtu := 1400
				p.Spec.Networks[0].Mtu = &mtu
			}
			if test.phase == "rolled-back" {
				failure := "original operation failed"
				p.Failure = &failure
			}
			encoded, _ = json.Marshal(p)
			op, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: "business", EnvironmentID: &env.ID, ScopeKind: "environment", ScopeID: env.ID, Kind: test.kind, Payload: encoded, ExpectedRevision: env.Revision})
			if err != nil {
				t.Fatal(err)
			}
			owner := "worker"
			claimed, err := q.ClaimOperation(ctx, queries.ClaimOperationParams{LeaseOwner: &owner})
			if err != nil {
				t.Fatal(err)
			}
			op = queries.Operation(claimed)
			worker := Worker{Pool: pool, Queries: q, Client: &transport.Client{HTTP: server.Client()}}
			if err := worker.phase(ctx, &op, &p, test.phase); err != nil {
				t.Fatal(err)
			}
			if test.phase == "rolled-back" {
				if _, err := pool.Exec(ctx, `UPDATE environments SET status='running',error='original operation failed' WHERE id='env'; UPDATE operations SET lease_until=now()-interval '1 minute' WHERE id='business'`); err != nil {
					t.Fatal(err)
				}
				reclaimed, err := q.ClaimOperation(ctx, queries.ClaimOperationParams{LeaseOwner: &owner})
				if err != nil {
					t.Fatal(err)
				}
				op = queries.Operation(reclaimed)
			}
			worker.execute(ctx, op)
			result, err := q.GetOperation(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if test.phase == "rolled-back" {
				after, err := q.GetEnvironment(ctx, env.ID)
				if err != nil || networks.Load() != 0 || after.Status != "running" || after.Error == nil || *after.Error != *p.Failure || string(after.AppliedSpec) != string(after.Spec) || after.Revision != env.Revision || strings.Contains(string(after.AppliedSpec), `"mtu"`) {
					t.Fatal("terminal rollback replayed failed desired configuration")
				}
				return
			}
			if test.reject {
				if result.State != "failed" || result.Phase != "rolled-back" || networks.Load() != 2 || result.Error == nil || !strings.Contains(*result.Error, "network replay rejected") {
					t.Fatalf("network failure bypassed standard rollback: %+v, calls=%d", result, networks.Load())
				}
			} else if result.State != "succeeded" {
				t.Fatalf("resume failed: %+v", result)
			}
			after, err := q.GetEnvironment(ctx, env.ID)
			if err != nil || after.Status != "running" || !strings.Contains(string(after.AppliedSpec), `"accessAddress"`) || !strings.Contains(string(after.AppliedSpec), `"routers"`) || string(after.Draft) != "{}" || after.Revision != env.Revision+1 {
				t.Fatalf("actual network metadata was not retained: status=%s revision=%d applied=%s draft=%s, %v", after.Status, after.Revision, after.AppliedSpec, after.Draft, err)
			}
			assets, err := q.ListRuntimeAssets(ctx, env.ID)
			if err != nil || len(assets) != 1 || assets[0].InstanceID != "original" || assets[0].State != "running" {
				t.Fatalf("existing execution changed: %+v, %v", assets, err)
			}
		})
	}
}
