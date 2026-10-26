package db

import (
	"context"
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCPUAllocationForwardMigration(t *testing.T) {
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
	schema := pgx.Identifier{"cpu_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")}.Sanitize()
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
		if entry.Name() >= "027_cpu_allocation.sql" {
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
	if _, err = pool.Exec(ctx, `INSERT INTO nodes(id,name,endpoint,info) VALUES('node','Node','https://node','{}');
 INSERT INTO environments(id,project_id,name,spec,status) VALUES('env','default','Environment','{}','running');
 INSERT INTO runtime_assets(environment_id,asset_id,instance_id,node_id,execution,cpu,memory_mib,disk_gib,current,state)
 VALUES('env','asset','instance','node','{"asset":{"resources":{"cpu":2}}}',2,512,1,true,'running')`); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var cpu, ratio float64
	var state string
	if err = pool.QueryRow(ctx, "SELECT a.cpu,n.cpu_allocation_ratio,a.state FROM runtime_assets a JOIN nodes n ON n.id=a.node_id").Scan(&cpu, &ratio, &state); err != nil || cpu != 2 || ratio != 4 || state != "running" {
		t.Fatalf("existing runtime changed: CPU=%g ratio=%g state=%s error=%v", cpu, ratio, state, err)
	}
	if _, err = pool.Exec(ctx, "UPDATE runtime_assets SET cpu=0.25"); err != nil {
		t.Fatal("fractional reservation rejected after upgrade", err)
	}
	if err = pool.QueryRow(ctx, "SELECT cpu FROM node_asset_reservations").Scan(&cpu); err != nil || cpu != 0.25 {
		t.Fatal("upgraded reservation view truncated CPU", cpu, err)
	}
}
