package observation

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"netlab.local/core/api"
	"netlab.local/core/db"
	"netlab.local/core/db/queries"
)

func TestNodeObservationsWithPostgreSQL(t *testing.T) {
	dsn := os.Getenv("NETLAB_DATABASE_URL")
	if dsn == "" {
		t.Skip("NETLAB_DATABASE_URL is not set")
	}
	ctx := context.Background()
	base, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	schema := pgx.Identifier{"observe_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")}.Sanitize()
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
	_, err = pool.Exec(ctx, `
	 INSERT INTO nodes(id,name,endpoint,info) VALUES('n1','One','https://one','{}'),('n2','Two','https://two','{}');
	 INSERT INTO environments(id,project_id,name,spec) VALUES('env','default','Environment','{"assets":[],"networks":[]}');
	 INSERT INTO runtime_assets(environment_id,asset_id,instance_id,node_id,execution,cpu,memory_mib,disk_gib,current,state,observed_at)
	 VALUES('env','one','old','n1','{}',1,512,1,false,'stopped','2020-01-01'),
	 ('env','one','i1','n1','{}',1,512,1,true,'running','2020-01-01'),
	 ('env','two','i2','n1','{}',1,512,1,true,'running','2020-01-01'),
	 ('env','three','i3','n2','{}',1,512,1,true,'running','2020-01-01');`)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	apply := func(node string, snapshot bool, at time.Time, results ...api.ExecutionResult) {
		t.Helper()
		if results == nil {
			results = []api.ExecutionResult{}
		}
		raw, err := json.Marshal(results)
		if err != nil {
			t.Fatal(err)
		}
		if err = q.ApplyNodeObservation(ctx, queries.ApplyNodeObservationParams{NodeID: node, Snapshot: snapshot, Results: raw, ObservedAt: pgtype.Timestamptz{Time: at, Valid: true}}); err != nil {
			t.Fatal(err)
		}
	}
	result := func(asset, instance, state string, when time.Time) api.ExecutionResult {
		environment := "env"
		return api.ExecutionResult{EnvironmentId: &environment, AssetId: asset, InstanceId: instance, State: state, ObservedAt: when}
	}
	read := func(instance string) queries.RuntimeAsset {
		t.Helper()
		rows, err := q.ListRuntimeAssets(ctx, "env")
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.InstanceID == instance {
				return row
			}
		}
		t.Fatalf("instance %s missing", instance)
		return queries.RuntimeAsset{}
	}
	countEvents := func() int64 {
		t.Helper()
		id := "env"
		cursor, err := q.CurrentEventCursor(ctx, &id)
		if err != nil {
			t.Fatal(err)
		}
		return cursor
	}
	apply("n1", false, at, result("one", "i1", "stopped", at), result("two", "i2", "suspended", at))
	if countEvents() != 1 || read("i1").State != "stopped" || read("i2").State != "suspended" {
		t.Fatal("batch did not update both assets and emit one environment event")
	}
	t.Run("same state advances time; older and foreign identities do not", func(t *testing.T) {
		next := at.Add(time.Second)
		message := "automatic restart failed"
		r := result("one", "i1", "stopped", next)
		r.Error = &message
		apply("n1", false, next, r)
		observed := read("i1")
		if !observed.ObservedAt.Time.Equal(next) || observed.Error == nil || *observed.Error != message {
			t.Fatalf("new observation was lost: %+v", observed)
		}
		apply("n1", false, next, result("one", "i1", "running", at))
		apply("n2", false, next, result("one", "i1", "running", next))
		apply("n1", false, next, result("wrong-asset", "i1", "running", next))
		r = result("one", "i1", "running", next)
		wrongEnvironment := "other"
		r.EnvironmentId = &wrongEnvironment
		apply("n1", false, next, r)
		apply("n1", false, next, result("one", "old", "running", next))
		if read("i1").State != "stopped" || read("old").State != "stopped" {
			t.Fatal("a stale, foreign or replaced instance event overwrote current state")
		}
		previous := countEvents()
		apply("n1", false, next.Add(time.Second))
		if countEvents() != previous || !read("i1").ObservedAt.Time.Equal(next) {
			t.Fatal("activity heartbeat rewrote runtime assets")
		}
	})
	t.Run("snapshot absence preserves newer instances and capacity", func(t *testing.T) {
		snapshotAt := at.Add(3 * time.Second)
		newer := snapshotAt.Add(time.Second)
		apply("n1", false, newer, result("two", "i2", "running", newer))
		apply("n1", true, snapshotAt, result("one", "i1", "stopped", snapshotAt))
		if read("i2").State != "running" {
			t.Fatal("snapshot scanned before the instance update marked it absent")
		}
		apply("n1", true, snapshotAt.Add(2*time.Second), result("one", "i1", "stopped", snapshotAt.Add(2*time.Second)))
		if read("i2").State != "absent" || read("i3").State != "running" {
			t.Fatal("snapshot did not limit missing instances to its node")
		}
		reserved, err := q.GetReservedResources(ctx, "n1")
		if err != nil || reserved.Cpu != 3 || reserved.MemoryMib != 1536 || reserved.DiskGib != 3 {
			t.Fatalf("observations released capacity: %+v %v", reserved, err)
		}
	})
	t.Run("disconnect preserves facts and time while state becomes unknown", func(t *testing.T) {
		before := read("i1")
		if err := q.MarkObservedNodeOffline(ctx, "n1"); err != nil {
			t.Fatal(err)
		}
		first := countEvents()
		if err := q.MarkObservedNodeOffline(ctx, "n1"); err != nil || countEvents() != first {
			t.Fatalf("repeated disconnect wrote duplicate event: %v", err)
		}
		states, err := q.ListRuntimeAssetStates(ctx, "env")
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range states {
			if row.NodeID == "n1" && (row.State != "unknown" || row.Error != "节点连接已断开") {
				t.Fatalf("disconnected instance still appeared confirmed: %+v", row)
			}
		}
		after := read("i1")
		if after.State != before.State || !after.ObservedAt.Time.Equal(before.ObservedAt.Time) {
			t.Fatal("disconnect overwrote the last runtime observation")
		}
		apply("n1", false, at.Add(10*time.Second))
		states, err = q.ListRuntimeAssetStates(ctx, "env")
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range states {
			if row.InstanceID == "i1" && row.State != "stopped" {
				t.Fatal("recovered node did not expose its confirmed state")
			}
		}
	})
}
