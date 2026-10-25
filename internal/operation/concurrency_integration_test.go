package operation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
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

func concurrencyPool(t *testing.T, connections int32) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("NETLAB_DATABASE_URL")
	if dsn == "" {
		t.Skip("NETLAB_DATABASE_URL selects PostgreSQL")
	}
	ctx := context.Background()
	base, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := pgx.Identifier{"concurrency_" + strings.ReplaceAll(uuid.NewString(), "-", "")}.Sanitize()
	if _, err = base.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { base.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); base.Close(ctx) })
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = connections
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestIndependentAdvisoryLocks(t *testing.T) {
	pool := concurrencyPool(t, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	leader, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer leader.Close(context.Background())
	if _, err = leader.Exec(ctx, "SELECT pg_advisory_lock(73421493),pg_advisory_lock(73421494)"); err != nil {
		t.Fatal(err)
	}
	q := queries.New(pool)
	if err = (Service{Pool: pool, Queries: q}).ConfigureManagedStorage(ctx, nil); err != nil {
		t.Fatalf("storage membership waited for the observer/update leader: %v", err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO principals(id,name,kind,administrator) VALUES('admin','admin','user',true);
	 INSERT INTO environments(id,project_id,name,spec) VALUES('env','default','Env','{"assets":[],"networks":[]}')`); err != nil {
		t.Fatal(err)
	}
	identity := access.Identity{Principal: queries.Principal{ID: "admin", Kind: "user", Administrator: true}}
	if err = (access.Service{Pool: pool, Queries: q}).UpdateUser(ctx, identity, "admin", api.UpdateUser{Name: "admin"}); err != nil {
		t.Fatalf("administrator edit waited for the updater: %v", err)
	}
	id := "env"
	w := Worker{Pool: pool, Queries: q}
	if _, _, err = w.prepareVPN(ctx, &queries.Operation{ID: "vpn", EnvironmentID: &id}, api.EnvironmentSpec{}, nil); err != nil {
		t.Fatalf("VPN planning waited for the updater: %v", err)
	}
}

func TestSharedTemplateDoesNotOccupyBusinessPool(t *testing.T) {
	pool := concurrencyPool(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
			json.NewEncoder(w).Encode(api.Template{})
		case <-r.Context().Done():
		}
	}))
	defer node.Close()
	defer close(release)
	w := Worker{Pool: pool, Queries: queries.New(pool), Client: &transport.Client{HTTP: node.Client()}}
	done := make(chan error, 1)
	go func() {
		done <- w.prepareSharedTemplate(ctx, "pool/template/1", node.URL, api.NodeTemplatePreparation{})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	request, finish := context.WithTimeout(ctx, 500*time.Millisecond)
	defer finish()
	if err := pool.Ping(request); err != nil {
		t.Fatalf("slow node consumed the sole business connection: %v", err)
	}
	wait, stop := context.WithCancel(ctx)
	blocked := make(chan error, 1)
	go func() {
		blocked <- w.prepareSharedTemplate(wait, "pool/template/1", node.URL, api.NodeTemplatePreparation{})
	}()
	stop()
	select {
	case err := <-blocked:
		if err == nil {
			t.Fatal("cancelled lock wait succeeded")
		}
	case <-ctx.Done():
		t.Fatal("cancelled shared preparation did not exit")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("active preparation did not observe cancellation")
	}
}

func TestLifecycleSubmissionAndRetryConcurrency(t *testing.T) {
	pool := concurrencyPool(t, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	q := queries.New(pool)
	s := environment.Service{Pool: pool, Queries: q}
	if err := q.CreatePrincipal(ctx, queries.CreatePrincipalParams{ID: "admin", Name: "admin", Kind: "user", Administrator: true}); err != nil {
		t.Fatal(err)
	}
	identity := access.Identity{Principal: queries.Principal{ID: "admin", Kind: "user", Administrator: true}}
	spec := api.EnvironmentSpec{Assets: []api.Asset{}, Networks: []api.Network{}}
	for _, stableKey := range []bool{true, false} {
		env, err := s.Create(ctx, identity, api.CreateEnvironment{Name: "Concurrent actions", Spec: &spec})
		if err != nil {
			t.Fatal(err)
		}
		type result struct {
			op  api.Operation
			err error
		}
		results := make(chan result, 32)
		for range 32 {
			go func() {
				key := "same-intent"
				if !stableKey {
					key = uuid.NewString()
				}
				op, err := s.Action(ctx, identity, env.Id, "", api.ActionRequest{Action: api.ActionRequestActionStart, ClientRequestId: &key})
				results <- result{op, err}
			}()
		}
		ids := map[string]bool{}
		accepted := 0
		var id string
		for range 32 {
			r := <-results
			if r.err == nil {
				accepted++
				id = r.op.Id
				ids[id] = true
			} else if !errors.Is(r.err, environment.ErrBusy) {
				t.Fatal(r.err)
			}
		}
		if len(ids) != 1 || stableKey && accepted != 32 || !stableKey && accepted != 1 {
			t.Fatalf("stable=%v accepted=%d operations=%d", stableKey, accepted, len(ids))
		}
		if _, err = pool.Exec(ctx, "UPDATE operations SET state='failed' WHERE id=$1", id); err != nil {
			t.Fatal(err)
		}
		service := Service{Pool: pool, Queries: q}
		for range 32 {
			go func() { op, err := service.Retry(ctx, identity, id); results <- result{op, err} }()
		}
		for range 32 {
			r := <-results
			if r.err != nil || r.op.Id != id {
				t.Fatalf("retry=%+v error=%v", r.op, r.err)
			}
		}
		conflicting := []api.ActionRequestAction{api.ActionRequestActionStop, api.ActionRequestActionDestroy, api.ActionRequestActionRebuild}
		for _, action := range conflicting {
			if _, err = s.Action(ctx, identity, env.Id, "", api.ActionRequest{Action: action}); !errors.Is(err, environment.ErrBusy) {
				t.Fatalf("conflict %s: %v", action, err)
			}
		}
		if _, err = pool.Exec(ctx, "UPDATE operations SET state='succeeded' WHERE id=$1", id); err != nil {
			t.Fatal(err)
		}
	}
}

func TestControlClaimAndConcurrentCapacity(t *testing.T) {
	pool := concurrencyPool(t, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	q := queries.New(pool)
	info := api.NodeInfo{Id: "node", Capabilities: []string{"container"}, Capacity: api.Resources{Cpu: 8, MemoryMiB: 1024, DiskGiB: 100}, Storage: &api.StorageInfo{Path: "/pool", Filesystem: "disk", CapacityBytes: 100 << 30, AvailableBytes: 100 << 30}}
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(info) }))
	defer node.Close()
	raw, _ := json.Marshal(info)
	if err := q.PutNode(ctx, queries.PutNodeParams{ID: "node", Name: "node", Endpoint: node.URL, Info: raw}); err != nil {
		t.Fatal(err)
	}
	ready := api.TemplateStateReady
	image := api.Template{Id: "image", Kind: api.Container, Version: 1, State: &ready}
	raw, _ = json.Marshal(image)
	if err := q.CreateTemplate(ctx, queries.CreateTemplateParams{ID: "image", Definition: raw}); err != nil {
		t.Fatal(err)
	}
	w := Worker{Pool: pool, Queries: q, Client: &transport.Client{HTTP: node.Client()}}
	owner := "concurrency-test"
	var waiting sync.WaitGroup
	planned := make(chan error, 12)
	for range 12 {
		spec := api.EnvironmentSpec{Networks: []api.Network{}, Assets: []api.Asset{}}
		for range 4 {
			spec.Assets = append(spec.Assets, api.Asset{Id: uuid.NewString(), Name: "Guest", TemplateId: "image", Resources: api.Resources{Cpu: 1, MemoryMiB: 128, DiskGiB: 1}})
		}
		raw, _ = json.Marshal(spec)
		env, err := q.CreateEnvironment(ctx, queries.CreateEnvironmentParams{ID: uuid.NewString(), ProjectID: "default", Name: "Capacity", Spec: raw})
		if err != nil {
			t.Fatal(err)
		}
		op, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: uuid.NewString(), EnvironmentID: &env.ID, ScopeKind: "environment", ScopeID: env.ID, Kind: "start", Payload: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, "UPDATE operations SET state='running',lease_owner=$2 WHERE id=$1", op.ID, owner); err != nil {
			t.Fatal(err)
		}
		op.LeaseOwner = &owner
		waiting.Go(func() { p := Payload{Spec: spec}; planned <- w.plan(ctx, &op, &p) })
	}
	waiting.Wait()
	succeeded := 0
	for range 12 {
		err := <-planned
		if err == nil {
			succeeded++
		} else if !strings.Contains(err.Error(), "没有节点可承载资产") {
			t.Fatal(err)
		}
	}
	reserved, err := q.GetReservedResources(ctx, "node")
	if err != nil || succeeded != 2 || reserved.Cpu != 8 || reserved.MemoryMib != 1024 {
		t.Fatalf("planned=%d reserved=%+v error=%v", succeeded, reserved, err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO operations(id,scope_kind,scope_id,kind,payload,expected_revision) VALUES('cold','template','cold','prepare-template','{}',0),('stop','environment','control','stop','{}',0)`); err != nil {
		t.Fatal(err)
	}
	op, err := q.ClaimOperation(ctx, queries.ClaimOperationParams{LeaseOwner: &owner, ControlOnly: true})
	if err != nil || op.ID != "stop" {
		t.Fatalf("control claimant selected %s: %v", op.ID, err)
	}
}

func TestBatchPersistsNodeResultsBeforeSlowNode(t *testing.T) {
	pool := concurrencyPool(t, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release := make(chan struct{})
	finish := sync.OnceFunc(func() { close(release) })
	defer finish()
	nodes := map[string]*httptest.Server{}
	for _, id := range []string{"fast", "slow"} {
		nodes[id] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var plan api.NodePlan
			if err := json.NewDecoder(r.Body).Decode(&plan); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			if id == "slow" {
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			result := api.NodeResult{Results: []api.ExecutionResult{}}
			for _, asset := range plan.Assets {
				result.Results = append(result.Results, api.ExecutionResult{AssetId: asset.Asset.Id, InstanceId: asset.InstanceId, State: "running", ObservedAt: time.Now().UTC()})
			}
			json.NewEncoder(w).Encode(result)
		}))
		defer nodes[id].Close()
	}
	defer finish()
	q := queries.New(pool)
	env, err := q.CreateEnvironment(ctx, queries.CreateEnvironmentParams{ID: uuid.NewString(), ProjectID: "default", Name: "Partial results", Spec: []byte(`{"assets":[],"networks":[]}`)})
	if err != nil {
		t.Fatal(err)
	}
	p := Payload{}
	for id, node := range nodes {
		if err = q.PutNode(ctx, queries.PutNodeParams{ID: id, Name: id, Endpoint: node.URL, Info: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
		a := api.AssetExecution{InstanceId: id, Asset: api.Asset{Id: id, Name: id}}
		raw, _ := json.Marshal(a)
		if err = q.ReserveAsset(ctx, queries.ReserveAssetParams{EnvironmentID: env.ID, AssetID: id, InstanceID: id, NodeID: id, Execution: raw}); err != nil {
			t.Fatal(err)
		}
		p.Targets = append(p.Targets, Target{NodeID: id, Execution: a})
	}
	op, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: uuid.NewString(), EnvironmentID: &env.ID, ScopeKind: "environment", ScopeID: env.ID, Kind: "start", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	owner := "batch-test"
	op.LeaseOwner = &owner
	op.Phase = "activate"
	if _, err = pool.Exec(ctx, "UPDATE operations SET state='running',lease_owner=$2 WHERE id=$1", op.ID, owner); err != nil {
		t.Fatal(err)
	}
	w := Worker{Pool: pool, Queries: q, Client: &transport.Client{HTTP: nodes["fast"].Client()}}
	done := make(chan error, 1)
	go func() { _, err := w.batch(ctx, &op, &p, api.NodePlanPhaseInspect, p.Targets); done <- err }()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var state string
		if err = pool.QueryRow(ctx, "SELECT state FROM runtime_assets WHERE instance_id='fast'").Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == "running" {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("batch finished before slow node released: %v", err)
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("fast node result was held behind slow node")
		}
	}
	finish()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
