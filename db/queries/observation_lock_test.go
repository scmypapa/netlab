package queries_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"netlab.local/core/db"
	"netlab.local/core/db/queries"
)

func TestObservationWhileEnvironmentLocked(t *testing.T) {
	dsn := os.Getenv("NETLAB_DATABASE_URL")
	if dsn == "" {
		t.Skip("NETLAB_DATABASE_URL is not set")
	}
	ctx := context.Background()
	base, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close(ctx)
	schema := pgx.Identifier{"observation_lock_" + strings.ReplaceAll(uuid.NewString(), "-", "")}.Sanitize()
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
	if _, err = pool.Exec(ctx, `
	 INSERT INTO environments(id,project_id,name,spec,status) VALUES('env','default','Env','{}','running');
	 INSERT INTO nodes(id,name,endpoint,info) VALUES('node','Node','https://test.invalid','{}');
	 INSERT INTO runtime_assets(environment_id,asset_id,instance_id,node_id,execution,state,cpu,memory_mib,disk_gib,current)
	 VALUES('env','asset','instance','node','{}','stopped',1,64,1,true)`); err != nil {
		t.Fatal(err)
	}
	control, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Rollback(ctx)
	if _, err = queries.New(control).LockEnvironment(ctx, "env"); err != nil {
		t.Fatal(err)
	}
	// A lifecycle write lock must allow the event foreign key's KEY SHARE lock.
	observation, finish := context.WithTimeout(ctx, time.Second)
	defer finish()
	now := time.Now()
	results, _ := json.Marshal([]map[string]any{{"environmentId": "env", "assetId": "asset", "instanceId": "instance", "state": "running", "observedAt": now}})
	if err = queries.New(pool).ApplyNodeObservation(observation, queries.ApplyNodeObservationParams{NodeID: "node", Results: results, ObservedAt: pgtype.Timestamptz{Time: now, Valid: true}}); err != nil {
		t.Fatalf("observation was blocked by lifecycle transaction: %v", err)
	}
	competitor, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer competitor.Release()
	if _, err = competitor.Exec(ctx, "SET lock_timeout='100ms'"); err != nil {
		t.Fatal(err)
	}
	defer competitor.Exec(ctx, "RESET lock_timeout")
	_, err = queries.New(competitor).LockEnvironment(ctx, "env")
	pg, ok := err.(*pgconn.PgError)
	if !ok || pg.Code != "55P03" {
		t.Fatalf("concurrent lifecycle mutation was not excluded: %v", err)
	}
}
