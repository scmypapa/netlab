package update

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
	"netlab.local/core/internal/transport"
)

func updateTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("NETLAB_DATABASE_URL")
	if dsn == "" {
		t.Skip("NETLAB_DATABASE_URL is not set")
	}
	ctx := context.Background()
	base, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := pgx.Identifier{"update_" + strings.ReplaceAll(uuid.NewString(), "-", "")}.Sanitize()
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		base.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() { base.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); base.Close(ctx) })
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.ConnConfig.RuntimeParams["application_name"] = "netlab-update-test"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestRolloutDrainAndClaim(t *testing.T) {
	pool := updateTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,spec,status) VALUES('env','default','Env','{}','running');
	 INSERT INTO operations(id,environment_id,scope_kind,scope_id,kind,payload,expected_revision,state)
	 VALUES('running','env','environment','env','apply','{}',0,'running'),('queued','env','environment','env','apply','{}',0,'queued')`)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	owner := "worker"
	type outcome struct {
		release func(error) error
		err     error
	}
	result := make(chan outcome, 1)
	go func() { release, err := Rollout(ctx, pool, nil, directory, "v1.1.0"); result <- outcome{release, err} }()
	for {
		data, err := os.ReadFile(directory + "/status.json")
		if err == nil && strings.Contains(string(data), "waiting_operations") {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err := queries.New(pool).ClaimOperation(ctx, &owner); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("maintenance allowed claim: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE operations SET state='succeeded' WHERE id='running'"); err != nil {
		t.Fatal(err)
	}
	completed := <-result
	if completed.err != nil {
		t.Fatal(completed.err)
	}
	defer completed.release(nil)
	if _, err := queries.New(pool).ClaimOperation(ctx, &owner); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("maintenance released before controller switch: %v", err)
	}
	if err := completed.release(nil); err != nil {
		t.Fatal(err)
	}
	if operation, err := queries.New(pool).ClaimOperation(ctx, &owner); err != nil || operation.ID != "queued" {
		t.Fatalf("queued task did not resume: %+v %v", operation, err)
	}
}

func TestRolloutNodeResult(t *testing.T) {
	for _, mode := range []string{"success", "failure", "wrong_identity", "wrong_version"} {
		t.Run(mode, func(t *testing.T) {
			pool := updateTestPool(t)
			var submitted atomic.Bool
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					var request api.ApplySystemUpdate
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Version != "v1.1.0" {
						t.Errorf("wrong update request: %+v %v", request, err)
					}
					submitted.Store(true)
					w.WriteHeader(http.StatusAccepted)
					return
				}
				if r.URL.Path == "/node/v1/info" {
					info := api.NodeInfo{Id: "node", Version: "v1.1.0"}
					if mode == "wrong_identity" {
						info.Id = "other"
					}
					if mode == "wrong_version" {
						info.Version = "v1.0.0"
					}
					json.NewEncoder(w).Encode(info)
					return
				}
				status := api.SystemUpdate{CurrentVersion: "v1.0.0", CanApply: true}
				if submitted.Load() {
					status.CurrentVersion = "v1.1.0"
					status.Activity = &api.UpdateActivity{Version: "v1.1.0", Phase: "succeeded"}
					if mode == "failure" {
						detail := "guacd restart failed"
						status.Activity.Phase, status.Activity.Error = "failed", &detail
					}
				}
				json.NewEncoder(w).Encode(status)
			}))
			defer endpoint.Close()
			ctx := context.Background()
			if err := queries.New(pool).PutNode(ctx, queries.PutNodeParams{ID: "node", Name: "A", Endpoint: endpoint.URL, Info: []byte(`{}`)}); err != nil {
				t.Fatal(err)
			}
			if mode != "success" {
				if err := queries.New(pool).PutNode(ctx, queries.PutNodeParams{ID: "z-later", Name: "B", Endpoint: "http://127.0.0.1:1", Info: []byte(`{}`)}); err != nil {
					t.Fatal(err)
				}
			}
			release, err := Rollout(ctx, pool, &transport.Client{HTTP: endpoint.Client()}, t.TempDir(), "v1.1.0")
			if release != nil {
				if err := release(nil); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "success" && err != nil || mode != "success" && err == nil {
				t.Fatalf("%s: %v", mode, err)
			}
			if mode != "success" && !strings.Contains(err.Error(), "A") {
				t.Fatalf("did not stop at failed node: %v", err)
			}
			var info []byte
			if err := pool.QueryRow(ctx, "SELECT info FROM nodes WHERE id='node'").Scan(&info); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(info), "v1.1.0") != (mode == "success") {
				t.Fatalf("incorrect persisted node fact: %s", info)
			}
		})
	}
}

func TestRolloutInterruptedResume(t *testing.T) {
	pool := updateTestPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,spec,status) VALUES('env','default','Env','{}','running');
	 INSERT INTO operations(id,environment_id,scope_kind,scope_id,kind,payload,expected_revision)
	 VALUES('queued','env','environment','env','apply','{}',0)`)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	finish, err := Rollout(ctx, pool, nil, directory, "v1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `SELECT pg_terminate_backend(l.pid) FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid
	 WHERE l.locktype='advisory' AND l.objid=73421494 AND l.mode='ExclusiveLock' AND l.granted AND a.application_name='netlab-update-test'`)
	if err != nil {
		t.Fatal(err)
	}
	if err := finish(errors.New("update connection lost")); err == nil {
		t.Fatal("lost update session was accepted")
	}
	owner := "worker"
	if _, err := queries.New(pool).ClaimOperation(ctx, &owner); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("claim resumed during interrupted update: %v", err)
	}
	if _, err := Rollout(ctx, pool, nil, directory, "v1.2.0"); err == nil {
		t.Fatal("different release replaced unfinished update")
	}
	finish, err = Rollout(ctx, pool, nil, directory, "v1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := finish(nil); err != nil {
		t.Fatal(err)
	}
	if operation, err := queries.New(pool).ClaimOperation(ctx, &owner); err != nil || operation.ID != "queued" {
		t.Fatalf("completed update did not resume claims: %+v %v", operation, err)
	}
}

func TestUpdateNodeResumesAcceptedRelease(t *testing.T) {
	var calls atomic.Int32
	var restarted atomic.Bool
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			restarted.Store(true)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		status := api.SystemUpdate{CurrentVersion: "v1.0.0", CanApply: true, Activity: &api.UpdateActivity{Version: "v1.1.0", Phase: "installing"}}
		if calls.Add(1) > 1 && restarted.Load() {
			status.CurrentVersion = "v1.1.0"
			status.Activity.Phase = "succeeded"
		}
		json.NewEncoder(w).Encode(status)
	}))
	defer endpoint.Close()
	if err := updateNode(context.Background(), &transport.Client{HTTP: endpoint.Client()}, endpoint.URL, "v1.1.0", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !restarted.Load() {
		t.Fatal("interrupted installer was not restarted")
	}
}

func TestResumePendingCurrentVersion(t *testing.T) {
	pool := updateTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE operations SET state='failed',error='node failed',payload='{"version":"v1.1.0"}' WHERE id='system-update'`); err != nil {
		t.Fatal(err)
	}
	service, err := New(Config{Repository: "example/netlab", Version: "v1.1.0", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	service.cfg.InstallDir = t.TempDir()
	service.Operations = queries.New(pool)
	service.latest = &api.UpdateRelease{Version: "v1.2.0"}
	starts := 0
	service.start = func(context.Context) error { starts++; return nil }
	status, err := service.Status(ctx)
	if err != nil || status.Activity == nil || status.Activity.Phase != "failed" || status.Activity.Version != "v1.1.0" {
		t.Fatalf("lost durable failure: %+v %v", status, err)
	}
	if err := service.Apply(ctx, "v1.2.0"); !errors.Is(err, ErrConflict) {
		t.Fatalf("skipped unfinished release: %v", err)
	}
	for range 2 {
		if err := service.Apply(ctx, "v1.1.0"); err != nil {
			t.Fatal(err)
		}
	}
	if starts != 2 {
		t.Fatalf("update unit was not resumed: %d", starts)
	}
	if _, err := pool.Exec(ctx, "UPDATE operations SET state='succeeded',error=NULL WHERE id='system-update'"); err != nil {
		t.Fatal(err)
	}
	if err := writeActivity(service.directory(), "v1.1.0", "failed", errors.New("stale progress")); err != nil {
		t.Fatal(err)
	}
	status, err = service.Status(ctx)
	if err != nil || status.Activity.Phase != "succeeded" || status.Activity.Error != nil {
		t.Fatalf("file overwrote completed operation: %+v %v", status, err)
	}
}

func TestClaimWaitsForMaintenanceWrite(t *testing.T) {
	pool := updateTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,spec,status) VALUES('env','default','Env','{}','running');
	 INSERT INTO operations(id,environment_id,scope_kind,scope_id,kind,payload,expected_revision) VALUES('queued','env','environment','env','apply','{}',0)`); err != nil {
		t.Fatal(err)
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback(ctx)
	if _, err := transaction.Exec(ctx, "UPDATE operations SET state='running' WHERE id='system-update'"); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { owner := "worker"; _, err := queries.New(pool).ClaimOperation(ctx, &owner); result <- err }()
	select {
	case err := <-result:
		t.Fatalf("claim ignored maintenance row lock: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err := transaction.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale snapshot admitted operation: %v", err)
	}
}
