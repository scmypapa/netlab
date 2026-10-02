package queries_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"netlab.local/core/db/queries"
)

func TestListCursorsAndFilters(t *testing.T) {
	dsn := os.Getenv("NETLAB_DATABASE_URL")
	if dsn == "" {
		t.Skip("NETLAB_DATABASE_URL is not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	q := queries.New(tx)
	prefix := fmt.Sprintf("list-test-%d-", time.Now().UnixNano())
	owner, other := prefix+"owner", prefix+"other"
	statements := []string{
		`INSERT INTO principals(id,name,kind) VALUES($1||'owner',$1||'owner','user'),($1||'other',$1||'other','user')`,
		`INSERT INTO environments(id,project_id,owner_id,name,external_reference,status,spec,applied_spec,created_at)
		 SELECT $1||lpad(n::text,3,'0'),'default',$1||'owner',$1||'environment-'||n,$1||'ref-'||n,
		 CASE WHEN n=1 THEN 'stopped' ELSE 'running' END,'{"assets":[{}],"networks":[]}',
		 '{"assets":[{},{}],"networks":[{}]}','2100-01-01' FROM generate_series(1,105) n`,
		`INSERT INTO templates(id,definition,created_at)
		 SELECT $1||lpad(n::text,3,'0'),jsonb_build_object('name',$1||'template-'||n,'os','Linux',
		 'kind',CASE WHEN n%2=0 THEN 'vm' ELSE 'container' END),'2100-01-01' FROM generate_series(1,105) n`,
		`INSERT INTO blueprints(id,project_id,owner_id,name,version,created_at)
		 SELECT $1||lpad(n::text,3,'0'),'default',$1||'owner',$1||'blueprint-'||n,1,'2100-01-01' FROM generate_series(1,105) n`,
		`INSERT INTO blueprint_versions(id,blueprint_id,version,spec,view,asset_count,network_count,source_environment_id,source_revision)
		 SELECT $1||'version-'||n,$1||lpad(n::text,3,'0'),1,'{"assets":[],"networks":[]}','{}',2,1,$1||'001',1 FROM generate_series(1,105) n`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement, prefix); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("environment summary cursor and authorization", func(t *testing.T) {
		params := queries.ListEnvironmentsParams{PrincipalID: &owner, Search: prefix, PageLimit: 100}
		first, err := q.ListEnvironments(ctx, params)
		if err != nil || len(first) != 100 {
			t.Fatalf("first page: count=%d error=%v", len(first), err)
		}
		if first[0].ID != prefix+"105" || first[99].ID != prefix+"006" || first[0].AssetCount != 2 || first[0].NetworkCount != 1 {
			t.Fatalf("incorrect ordering or applied counts: %v / %v", first[0], first[99])
		}
		params.Cursor = first[99].ID
		second, err := q.ListEnvironments(ctx, params)
		if err != nil || len(second) != 5 || second[0].ID != prefix+"005" || second[4].ID != prefix+"001" {
			t.Fatalf("second page: rows=%v error=%v", second, err)
		}
		params.Cursor, params.Search, params.Status = "", prefix+"ref-1", "stopped"
		filtered, err := q.ListEnvironments(ctx, params)
		if err != nil || len(filtered) != 1 || filtered[0].ID != prefix+"001" {
			t.Fatalf("reference and status: rows=%v error=%v", filtered, err)
		}
		params.Search, params.Status, params.PrincipalID = prefix, "", &other
		denied, err := q.ListEnvironments(ctx, params)
		if err != nil || len(denied) != 0 {
			t.Fatalf("ungranted principal: rows=%v error=%v", denied, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO grants VALUES($1,'environment',$2,ARRAY['read'])`, other, prefix+"105"); err != nil {
			t.Fatal(err)
		}
		allowed, err := q.ListEnvironments(ctx, params)
		if err != nil || len(allowed) != 1 || allowed[0].ID != prefix+"105" {
			t.Fatalf("environment grant: rows=%v error=%v", allowed, err)
		}
	})
	t.Run("template cursor kind and exact references", func(t *testing.T) {
		params := queries.ListTemplatePageParams{Search: prefix, Ids: []string{}, PageLimit: 100}
		first, err := q.ListTemplatePage(ctx, params)
		if err != nil || len(first) != 100 || first[99].ID != prefix+"006" {
			t.Fatalf("first page: count=%d error=%v", len(first), err)
		}
		params.Cursor = first[99].ID
		second, err := q.ListTemplatePage(ctx, params)
		if err != nil || len(second) != 5 || second[4].ID != prefix+"001" {
			t.Fatalf("second page: rows=%v error=%v", second, err)
		}
		params.Cursor, params.Kind = "", "vm"
		filtered, err := q.ListTemplatePage(ctx, params)
		if err != nil || len(filtered) != 52 {
			t.Fatalf("kind filter: count=%d error=%v", len(filtered), err)
		}
		params.Kind, params.Search, params.Ids = "", "", []string{prefix + "001", prefix + "105"}
		referenced, err := q.ListTemplatePage(ctx, params)
		if err != nil || len(referenced) != 2 || referenced[0].ID != prefix+"105" || referenced[1].ID != prefix+"001" {
			t.Fatalf("exact reference filter: rows=%v error=%v", referenced, err)
		}
	})
	t.Run("blueprint cursor and ownership", func(t *testing.T) {
		params := queries.ListBlueprintsParams{PrincipalID: &owner, Search: prefix, PageLimit: 100}
		first, err := q.ListBlueprints(ctx, params)
		if err != nil || len(first) != 100 || first[99].ID != prefix+"006" {
			t.Fatalf("first page: count=%d error=%v", len(first), err)
		}
		params.Cursor = first[99].ID
		second, err := q.ListBlueprints(ctx, params)
		if err != nil || len(second) != 5 || second[4].ID != prefix+"001" {
			t.Fatalf("second page: rows=%v error=%v", second, err)
		}
		params.Cursor, params.PrincipalID = "", &other
		denied, err := q.ListBlueprints(ctx, params)
		if err != nil || len(denied) != 0 {
			t.Fatalf("ungranted principal: count=%d error=%v", len(denied), err)
		}
	})
}
