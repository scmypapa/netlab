package queries_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"netlab.local/core/db/queries"
)

func TestRuntimeWritesWhileObserved(t *testing.T) {
	for _, kind := range []string{"release", "results", "execution", "reservation", "artifact", "ownership"} {
		t.Run(kind, func(t *testing.T) {
			pool := observationDatabase(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			seedObservedAssets(t, ctx, pool)
			_, err := pool.Exec(ctx, `
			 CREATE FUNCTION pause_runtime_write() RETURNS trigger LANGUAGE plpgsql AS $$
			 BEGIN
			  IF OLD.instance_id='a' AND current_setting('application_name')='runtime-lock-writer'
			  THEN PERFORM pg_advisory_xact_lock(749119,pg_backend_pid()); END IF;
			  IF TG_OP='DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
			 END $$;
			 CREATE TRIGGER pause_runtime_write BEFORE UPDATE OR DELETE ON runtime_assets FOR EACH ROW EXECUTE FUNCTION pause_runtime_write();`)
			if err != nil {
				t.Fatal(err)
			}
			writer, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Release()
			observer, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer observer.Release()
			for _, connection := range []*pgxpool.Conn{writer, observer} {
				// Expose the legal index plans that lock heap and instance rows in different orders.
				if _, err = connection.Exec(ctx, "SET enable_seqscan=off; SET enable_bitmapscan=off; SET enable_hashjoin=off; SET enable_mergejoin=off"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = writer.Exec(ctx, "SET application_name='runtime-lock-writer'"); err != nil {
				t.Fatal(err)
			}
			barrier, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer barrier.Release()
			pid := int32(writer.Conn().PgConn().PID())
			if _, err = barrier.Exec(ctx, "SELECT pg_advisory_lock(749119,$1)", pid); err != nil {
				t.Fatal(err)
			}
			defer barrier.Exec(context.Background(), "SELECT pg_advisory_unlock_all()")
			written := make(chan error, 1)
			go func() { written <- runtimeWrite(ctx, writer, kind) }()
			waitForBlocker(t, ctx, barrier, writer.Conn().PgConn().PID(), barrier.Conn().PgConn().PID())
			observed := make(chan error, 1)
			go func() { observed <- queries.New(observer).ApplyNodeObservation(ctx, assetObservation()) }()
			waitForBlocker(t, ctx, barrier, observer.Conn().PgConn().PID(), writer.Conn().PgConn().PID())
			if _, err = barrier.Exec(ctx, "SELECT pg_advisory_unlock(749119,$1)", pid); err != nil {
				t.Fatal(err)
			}
			if err = <-written; err != nil {
				t.Fatalf("runtime write failed: %v", err)
			}
			if err = <-observed; err != nil {
				t.Fatalf("observation failed: %v", err)
			}
			var count int
			if err = pool.QueryRow(ctx, "SELECT count(*) FROM runtime_assets WHERE instance_id IN ('a','b')").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if kind == "release" && count != 0 || kind != "release" && count != 2 {
				t.Fatalf("incorrect reservation count: %d", count)
			}
		})
	}
}

func TestObservationLocksNodeBeforeAssets(t *testing.T) {
	pool := observationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	seedObservedAssets(t, ctx, pool)
	writer, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Release()
	tx, err := writer.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = queries.New(tx).LockNode(ctx, "node"); err != nil {
		t.Fatal(err)
	}
	observer, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Release()
	observed := make(chan error, 1)
	go func() { observed <- queries.New(observer).ApplyNodeObservation(ctx, assetObservation()) }()
	monitor, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer monitor.Release()
	waitForBlocker(t, ctx, monitor, observer.Conn().PgConn().PID(), writer.Conn().PgConn().PID())
	if err = queries.New(tx).ReserveResourceUpdates(ctx, []byte(`[{"instanceId":"a","cpu":2,"memoryMiB":128,"diskGiB":2}]`)); err != nil {
		t.Fatalf("observer locked assets before the node: %v", err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-observed; err != nil {
		t.Fatal(err)
	}
}

func TestObservationRechecksStateAfterLockWait(t *testing.T) {
	for _, kind := range []string{"newer-result", "retired-instance"} {
		t.Run(kind, func(t *testing.T) {
			pool := observationDatabase(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			seedObservedAssets(t, ctx, pool)
			writer, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Release()
			tx, err := writer.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			q := queries.New(tx)
			observation := assetObservation()
			if kind == "newer-result" {
				newer, _ := json.Marshal([]map[string]any{{"assetId": "a", "instanceId": "a", "nodeId": "node", "state": "stopped", "observedAt": observation.ObservedAt.Time.Add(time.Second)}})
				err = q.ApplyAssetResults(ctx, newer)
			} else {
				err = q.ClearCurrentAssets(ctx, queries.ClearCurrentAssetsParams{EnvironmentID: "env", Column2: []string{"a"}})
			}
			if err != nil {
				t.Fatal(err)
			}
			observer, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer observer.Release()
			observed := make(chan error, 1)
			go func() { observed <- queries.New(observer).ApplyNodeObservation(ctx, observation) }()
			monitor, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer monitor.Release()
			waitForBlocker(t, ctx, monitor, observer.Conn().PgConn().PID(), writer.Conn().PgConn().PID())
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err = <-observed; err != nil {
				t.Fatal(err)
			}
			var state string
			if err = pool.QueryRow(ctx, "SELECT state FROM runtime_assets WHERE instance_id='a'").Scan(&state); err != nil {
				t.Fatal(err)
			}
			if kind == "newer-result" && state != "stopped" || kind == "retired-instance" && state != "running" {
				t.Fatalf("observation overwrote %s: %s", kind, state)
			}
		})
	}
}

func seedObservedAssets(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, `
	 INSERT INTO environments(id,project_id,name,spec,status) VALUES('env','default','Env','{}','running');
	 INSERT INTO nodes(id,name,endpoint,info) VALUES('node','Node','https://test.invalid','{}');
	 INSERT INTO runtime_assets(environment_id,asset_id,instance_id,node_id,execution,state,cpu,memory_mib,disk_gib,current)
	 VALUES('env','b','b','node','{"template":{"artifactNodeId":"source"}}','running',1,64,1,true),
	 ('env','a','a','node','{"template":{"artifactNodeId":"source"}}','running',1,64,1,true),
	 ('env','b','0-new-b','node','{}','reserved',1,64,1,false),
	 ('env','a','0-new-a','node','{}','reserved',1,64,1,false);
	 INSERT INTO operations(id,environment_id,scope_kind,scope_id,kind,state,payload,expected_revision)
	 VALUES('change','env','environment','env','change','running',
	 '{"targets":[{"execution":{"instanceId":"0-new-a"}},{"execution":{"instanceId":"0-new-b"}}]}',0);
	 UPDATE environments SET operation_id='change' WHERE id='env'`)
	if err != nil {
		t.Fatal(err)
	}
}

func assetObservation() queries.ApplyNodeObservationParams {
	results := []map[string]any{}
	now := time.Now().Add(time.Second)
	for _, id := range []string{"b", "a", "0-new-b", "0-new-a"} {
		asset := id[len(id)-1:]
		results = append(results, map[string]any{"environmentId": "env", "assetId": asset, "instanceId": id, "nodeId": "node", "state": "running", "observedAt": now})
	}
	raw, _ := json.Marshal(results)
	return queries.ApplyNodeObservationParams{NodeID: "node", Results: raw, ObservedAt: pgtype.Timestamptz{Time: now, Valid: true}}
}

func runtimeWrite(ctx context.Context, connection *pgxpool.Conn, kind string) error {
	tx, err := connection.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	q := queries.New(tx)
	switch kind {
	case "release":
		err = q.ReleaseAssets(ctx, []string{"a", "b"})
	case "results":
		err = q.ApplyAssetResults(ctx, assetObservation().Results)
	case "execution":
		err = q.UpdateExecutions(ctx, []byte(`[{"instanceId":"a","execution":{},"cpu":2,"memoryMiB":128,"diskGiB":2},{"instanceId":"b","execution":{},"cpu":2,"memoryMiB":128,"diskGiB":2}]`))
	case "reservation":
		err = q.ReserveResourceUpdates(ctx, []byte(`[{"instanceId":"a","cpu":2,"memoryMiB":128,"diskGiB":2},{"instanceId":"b","cpu":2,"memoryMiB":128,"diskGiB":2}]`))
	case "artifact":
		err = q.RelocateTemplateArtifacts(ctx, queries.RelocateTemplateArtifactsParams{Source: "source", Target: "destination"})
	case "ownership":
		if _, err = q.LockEnvironment(ctx, "env"); err == nil {
			_, err = q.LockEnvironmentAssets(ctx, "env")
		}
		if err == nil {
			err = q.ClearCurrentAssets(ctx, queries.ClearCurrentAssetsParams{EnvironmentID: "env", Column2: []string{"a", "b"}})
		}
		if err == nil {
			err = q.MakeCurrentAssets(ctx, []string{"0-new-a", "0-new-b"})
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func waitForBlocker(t *testing.T, ctx context.Context, monitor *pgxpool.Conn, waiter, blocker uint32) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := monitor.QueryRow(ctx, "SELECT $1::integer=ANY(pg_blocking_pids($2))", blocker, waiter).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("backend %d did not wait for %d: %v", waiter, blocker, ctx.Err())
		case <-ticker.C:
		}
	}
}
