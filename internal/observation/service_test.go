package observation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"netlab.local/core/api"
	"netlab.local/core/db"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/capture"
)

type packetQueryCount struct{ queries, batches atomic.Int64 }

func (c *packetQueryCount) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.queries.Add(1)
	return ctx
}
func (*packetQueryCount) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func (c *packetQueryCount) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	c.batches.Add(1)
	return ctx
}
func (*packetQueryCount) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}
func (*packetQueryCount) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData)     {}

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
	traced := &packetQueryCount{}
	config.ConnConfig.Tracer = traced
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
	t.Run("packet quota migration waits for source acknowledgement", func(t *testing.T) {
		service := Service{Pool: pool}
		targets := []api.SecurityInterface{{EnvironmentId: "env", Interface: api.CaptureInterface{NodeId: "n1"}}}
		budget, err := service.packetQuotas(ctx, targets)
		if err != nil || budget["n1"].requested["env"] != capture.PacketBudget {
			t.Fatal(budget, err)
		}
		observation, _ := json.Marshal(api.PacketRetention{LimitBytes: capture.PacketBudget, UsedBytes: 1024, Status: api.PacketRetentionStatusRecording})
		if err = q.SavePacketNode(ctx, queries.SavePacketNodeParams{EnvironmentID: "env", NodeID: "n1", QuotaBytes: capture.PacketBudget, Observation: observation}); err != nil {
			t.Fatal(err)
		}
		targets[0].Interface.NodeId = "n2"
		budget, err = service.packetQuotas(ctx, targets)
		if err != nil || budget["n1"].requested["env"] != capture.PacketBudget/2 || budget["n2"].requested["env"] != 0 {
			t.Fatalf("source capacity reused before acknowledgement: %+v %v", budget, err)
		}
		observation, _ = json.Marshal(api.PacketRetention{LimitBytes: capture.PacketBudget / 2, UsedBytes: 1024, Status: api.PacketRetentionStatusIdle})
		if err = q.SavePacketNode(ctx, queries.SavePacketNodeParams{EnvironmentID: "env", NodeID: "n1", QuotaBytes: capture.PacketBudget / 2, Observation: observation}); err != nil {
			t.Fatal(err)
		}
		budget, err = service.packetQuotas(ctx, targets)
		if err != nil || budget["n1"].reserved["env"]+budget["n2"].reserved["env"] != capture.PacketBudget || budget["n2"].requested["env"] != capture.PacketBudget/2 {
			t.Fatal(budget, err)
		}
	})
	t.Run("packet quotas use constant database round trips across environments", func(t *testing.T) {
		_, err := pool.Exec(ctx, `
		 DELETE FROM packet_nodes WHERE environment_id='env';
		 INSERT INTO environments(id,project_id,name,spec)
		 SELECT 'scale_'||n,'default','Scale '||n,'{"assets":[],"networks":[]}' FROM generate_series(1,128) n;
		 INSERT INTO nodes(id,name,endpoint,info) VALUES('n3','Three','https://three','{}'),('n4','Four','https://four','{}');`)
		if err != nil {
			t.Fatal(err)
		}
		targets := make([]api.SecurityInterface, 0, 1024)
		for i := 1; i <= 128; i++ {
			for _, node := range []string{"n1", "n2", "n3", "n4"} {
				target := api.SecurityInterface{EnvironmentId: fmt.Sprintf("scale_%d", i), Interface: api.CaptureInterface{NodeId: node}}
				targets = append(targets, target, target)
			}
		}
		service := Service{Pool: pool}
		for cycle := range 2 {
			traced.queries.Store(0)
			traced.batches.Store(0)
			started := time.Now()
			budgets, err := service.packetQuotas(ctx, targets)
			if err != nil {
				t.Fatal(err)
			}
			if traced.queries.Load() != 3 || traced.batches.Load() != int64(1-cycle) {
				t.Fatalf("cycle %d: %d queries, %d batches", cycle, traced.queries.Load(), traced.batches.Load())
			}
			for _, target := range targets {
				budget := budgets[target.Interface.NodeId]
				if budget.requested[target.EnvironmentId] != capture.PacketBudget/4 || budget.reserved[target.EnvironmentId] != capture.PacketBudget/4 {
					t.Fatalf("incorrect environment budget: %+v", target)
				}
			}
			t.Logf("128 environments, 4 nodes, cycle %d: 3 queries, %d batch, %v", cycle, traced.batches.Load(), time.Since(started))
		}
		_, err = pool.Exec(ctx, `
		 UPDATE packet_nodes SET quota_bytes=0 WHERE environment_id LIKE 'scale_%';
		 CREATE FUNCTION reject_quota() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN RAISE EXCEPTION 'quota write rejected'; END $$;
		 CREATE TRIGGER reject_quota BEFORE UPDATE ON packet_nodes FOR EACH ROW
		 WHEN (NEW.environment_id='scale_64' AND NEW.node_id='n2') EXECUTE FUNCTION reject_quota();`)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = service.packetQuotas(ctx, targets); err == nil || !strings.Contains(err.Error(), "quota write rejected") {
			t.Fatalf("batch failure was hidden: %v", err)
		}
		var reserved int64
		if err = pool.QueryRow(ctx, "SELECT sum(quota_bytes) FROM packet_nodes WHERE environment_id LIKE 'scale_%'").Scan(&reserved); err != nil || reserved != 0 {
			t.Fatalf("failed batch partially granted quota: %d %v", reserved, err)
		}
		if _, err = pool.Exec(ctx, "DROP TRIGGER reject_quota ON packet_nodes; DROP FUNCTION reject_quota()"); err != nil {
			t.Fatal(err)
		}
		if _, err = service.packetQuotas(ctx, targets); err != nil {
			t.Fatalf("batch failure did not release the connection: %v", err)
		}
	})
}
