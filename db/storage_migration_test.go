package db

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
)

func TestStorageBudgetForwardMigrationAndCleanup(t *testing.T) {
	dsn := os.Getenv("NETLAB_DATABASE_URL")
	if dsn == "" {
		t.Skip("NETLAB_DATABASE_URL selects PostgreSQL")
	}
	ctx := context.Background()
	base, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close(ctx)
	schema := pgx.Identifier{"storage_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")}.Sanitize()
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
	if _, err = pool.Exec(ctx, "CREATE TABLE schema_migrations(name text PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= "029_storage_budgets.sql" {
			break
		}
		source, err := migrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, string(source)); err != nil {
			t.Fatal(entry.Name(), err)
		}
		if _, err = pool.Exec(ctx, "INSERT INTO schema_migrations VALUES($1)", entry.Name()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = pool.Exec(ctx, `INSERT INTO nodes(id,name,endpoint,info) VALUES
 ('one','One','https://one','{"storage":{"path":"/node/data"}}'),('two','Two','https://two','{}');
 INSERT INTO storage_pools(id,node_ids,name,directory,path,driver) VALUES('data',ARRAY['one','two'],'Data','/data','/data','directory');
 INSERT INTO environments(id,project_id,name,spec,status) VALUES
 ('migration','default','Migration','{}','changing'),('restore','default','Restore','{}','changing'),('pending','default','Pending','{}','changing');
 INSERT INTO persistent_volumes(id,node_id,storage_pool_id,name,kind,size_gib,state) VALUES('pv','two','data','Volume','container',8,'ready')`); err != nil {
		t.Fatal(err)
	}
	type target struct {
		NodeID    string             `json:"nodeId"`
		Execution api.AssetExecution `json:"execution"`
	}
	path, dataPool := "/node/data", "data"
	source := api.AssetExecution{InstanceId: "old", StoragePath: &path, Template: api.Template{Kind: api.Container},
		Asset:         api.Asset{Id: "server", Resources: api.Resources{Cpu: 1, MemoryMiB: 128, DiskGiB: 3}, Volumes: &[]api.Volume{{Id: "anonymous", SizeGiB: 2}}},
		VolumeSources: map[string]api.NodeVolume{"persistent": {Id: "pv", Kind: api.Container, SizeGiB: 8, Storage: api.StorageInfo{Path: "/data"}}}}
	destination := source
	destination.InstanceId, destination.StoragePath, destination.StoragePoolId = "new", &dataPool, &dataPool
	before, after := target{"one", source}, target{"two", destination}
	marshal := func(value any) []byte {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	insertOperation := func(id, kind, phase string, payload any) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO operations(id,environment_id,scope_kind,scope_id,asset_id,kind,phase,payload,expected_revision)
 VALUES($1,$1,'environment',$1,'server',$2,$3,$4,0)`, id, kind, phase, marshal(payload)); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "UPDATE environments SET operation_id=$1 WHERE id=$1", id); err != nil {
			t.Fatal(err)
		}
	}
	insertOperation("migration", "migrate", "migration-release-capture", map[string]any{
		"before": []target{before}, "migration": map[string]any{"target": after, "live": false}, "committed": true,
		"recovery": map[string]any{"captures": map[string]any{"server": map[string]any{"sizeBytes": int64(9 << 30)}}},
	})
	if _, err := pool.Exec(ctx, `INSERT INTO runtime_assets(environment_id,asset_id,instance_id,node_id,execution,cpu,memory_mib,disk_gib,current)
 VALUES('migration','server','new','two',$1,1,128,5,true)`, marshal(destination)); err != nil {
		t.Fatal(err)
	}
	oldRestore, nextRestore := source, source
	oldRestore.InstanceId, nextRestore.InstanceId = "restoring", "restoring"
	nextRestore.Asset.Resources.DiskGiB = 4
	nextRestore.Asset.Volumes = &[]api.Volume{{Id: "anonymous", SizeGiB: 4}}
	legacy := nextRestore
	legacy.Asset.Resources.DiskGiB = 9
	insertOperation("restore", "restore-recovery", "cleanup", map[string]any{
		"before": []target{{"one", oldRestore}}, "updates": []target{{"one", nextRestore}}, "committed": true,
	})
	if _, err := pool.Exec(ctx, `INSERT INTO runtime_assets(environment_id,asset_id,instance_id,node_id,execution,cpu,memory_mib,disk_gib,current)
 VALUES('restore','server','restoring','one',$1,1,128,13,true)`, marshal(legacy)); err != nil {
		t.Fatal(err)
	}
	frames := map[string]any{"marker": "keep", "before": []target{before}, "targets": []target{before}, "updates": []target{before}, "unchanged": []target{before}, "old": []target{before}}
	insertOperation("pending", "change", "cleanup", frames)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	q := queries.New(pool)
	row, err := q.GetCurrentAsset(ctx, queries.GetCurrentAssetParams{EnvironmentID: "restore", AssetID: "server"})
	if err != nil {
		t.Fatal(err)
	}
	var restored api.AssetExecution
	if err := json.Unmarshal(row.Execution, &restored); err != nil || row.SystemGib != 7 || row.DiskGib != 13 || restored.Asset.Resources.DiskGiB != 4 {
		t.Fatalf("legacy recovery budget still falsifies guest configuration: %+v %+v %v", row, restored, err)
	}
	var pending map[string]json.RawMessage
	op, err := q.GetOperation(ctx, "pending")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(op.Payload, &pending); err != nil || string(pending["marker"]) != `"keep"` {
		t.Fatal("operation payload changed outside bindings", string(op.Payload), err)
	}
	for _, key := range []string{"before", "targets", "updates", "unchanged", "old"} {
		var targets []target
		if err := json.Unmarshal(pending[key], &targets); err != nil || len(targets) != 1 || targets[0].Execution.VolumeSources["persistent"].StoragePoolId != "data" {
			t.Fatal("pending operation could overwrite upgraded binding", key, targets, err)
		}
	}
	if _, err := pool.Exec(ctx, "DELETE FROM runtime_assets WHERE environment_id='restore'"); err != nil {
		t.Fatal(err)
	}
	assertBudget := func(phase string, want map[string][2]int64) {
		t.Helper()
		if _, err := pool.Exec(ctx, "UPDATE operations SET phase=$1 WHERE id='migration'", phase); err != nil {
			t.Fatal(err)
		}
		rows, err := q.StorageReservations(ctx, []string{"one", "two"})
		if err != nil {
			t.Fatal(err)
		}
		got := map[string][2]int64{}
		for _, row := range rows {
			got[row.NodeID+"/"+row.PoolID] = [2]int64{row.DiskGib, row.ContainerGib}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s released physical storage before cleanup: got=%v want=%v", phase, got, want)
		}
	}
	assertBudget("migration-release-capture", map[string][2]int64{"one/": {11, 3}, "one/data": {8, 0}, "two/data": {10, 3}})
	assertBudget("migration-cleanup", map[string][2]int64{"one/": {2, 3}, "one/data": {8, 0}, "two/data": {10, 3}})
	assertBudget("migration-services", map[string][2]int64{"two/data": {10, 3}})
	assertBudget("complete", map[string][2]int64{"two/data": {10, 3}})
	var upgradedSource []byte
	if err := pool.QueryRow(ctx, "SELECT payload->'before'->0->'execution' FROM operations WHERE id='migration'").Scan(&upgradedSource); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE runtime_assets SET instance_id='old',node_id='one',execution=$1 WHERE environment_id='migration'", upgradedSource); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE persistent_volumes SET node_id='one';
 UPDATE operations SET payload=jsonb_set(payload,'{committed}','false') WHERE id='migration'`); err != nil {
		t.Fatal(err)
	}
	assertBudget("migration-copy", map[string][2]int64{"one/": {11, 3}, "one/data": {8, 0}, "two/data": {19, 3}})
	assertBudget("migration-rollback", map[string][2]int64{"one/": {11, 3}, "one/data": {8, 0}})
	assertBudget("rolled-back", map[string][2]int64{"one/": {2, 3}, "one/data": {8, 0}})
	if _, err := pool.Exec(ctx, `UPDATE operations SET payload=jsonb_set(jsonb_set(payload,'{committed}','true'),'{migration,target,nodeId}','"one"') WHERE id='migration';
 UPDATE runtime_assets SET instance_id='new',node_id='one',execution=(SELECT payload->'migration'->'target'->'execution' FROM operations WHERE id='migration') WHERE environment_id='migration'`); err != nil {
		t.Fatal(err)
	}
	assertBudget("migration-cleanup", map[string][2]int64{"one/": {2, 3}, "one/data": {10, 3}})
}
