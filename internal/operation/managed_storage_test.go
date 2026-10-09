package operation

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"netlab.local/core/api"
	"netlab.local/core/db"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/transport"
)

func TestManagedStorageMembershipAndRetry(t *testing.T) {
	dsn := os.Getenv("NETLAB_DATABASE_URL")
	if dsn == "" {
		t.Skip("NETLAB_DATABASE_URL selects PostgreSQL")
	}
	ctx := context.Background()
	base, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	schema := pgx.Identifier{"storage_" + strings.ReplaceAll(uuid.NewString(), "-", "")}.Sanitize()
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
	q, service := queries.New(pool), Service{Pool: pool, Queries: queries.New(pool)}
	joined, fail := 0, true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/bootstrap"):
			json.NewEncoder(w).Encode(api.NodeCephBootstrap{PublicKey: "test-public-key"})
		case strings.HasSuffix(r.URL.Path, "/join"):
			joined++
			json.NewEncoder(w).Encode(api.NodeCephHost{Name: "test", Address: "127.0.0.1"})
		case strings.HasSuffix(r.URL.Path, "/configure"):
			key := "test-client-key"
			json.NewEncoder(w).Encode(api.CephConnection{Pool: "netlab", User: "netlab", Monitors: []string{"127.0.0.1:3300"}, Key: &key})
		default:
			if joined == 3 && fail {
				fail = false
				http.Error(w, "registration failed", 503)
				return
			}
			json.NewEncoder(w).Encode(api.StorageInfo{Path: "/test/storage", CapacityBytes: 8 << 30, AvailableBytes: 8 << 30})
		}
	}))
	defer server.Close()
	put := func(id string, device *string) {
		raw, _ := json.Marshal(api.NodeInfo{Id: id, Capabilities: []string{"vm"}, StorageDevice: device})
		if err := q.PutNode(ctx, queries.PutNodeParams{ID: id, Name: id, Endpoint: server.URL + "/" + id, Info: raw}); err != nil {
			t.Fatal(err)
		}
	}
	device := "/dev/disk/by-id/test"
	put("a", nil)
	if err = service.ConfigureManagedStorage(ctx, nil); err != nil {
		t.Fatal(err)
	}
	rows, err := q.ListStoragePools(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatal("single node unexpectedly initialized Ceph", err)
	}
	put("b", &device)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := service.ConfigureManagedStorage(ctx, nil); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	rows, err = q.ListStoragePools(ctx)
	if err != nil || len(rows) != 1 || rows[0].NodeIds[0] != "b" {
		t.Fatal("cluster publication was duplicated or owner changed", err)
	}
	put("c", nil)
	worker := Worker{Pool: pool, Queries: q, Client: &transport.Client{HTTP: server.Client()}}
	owner := "test"
	claimed, err := q.ClaimOperation(ctx, &owner)
	if err != nil {
		t.Fatal(err)
	}
	worker.execute(ctx, queries.Operation(claimed))
	claimed, err = q.ClaimOperation(ctx, &owner)
	if err != nil {
		t.Fatal("node registered during preparation was omitted", err)
	}
	worker.execute(ctx, queries.Operation(claimed))
	current, err := q.GetStoragePool(ctx, rows[0].ID)
	if err != nil || current.State != "ready" || len(current.NodeIds) != 2 {
		t.Fatal("failed expansion interrupted the existing pool", err)
	}
	op, err := q.GetOperation(ctx, claimed.ID)
	if err != nil || op.State != "failed" || bytes.Contains(op.Payload, []byte("test-client-key")) {
		t.Fatal("failure or credential boundary was lost", err)
	}
	identity := access.Identity{Principal: queries.Principal{Kind: "user", Administrator: true}}
	if _, err = service.Retry(ctx, identity, op.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err = q.ClaimOperation(ctx, &owner)
	if err != nil {
		t.Fatal(err)
	}
	worker.execute(ctx, queries.Operation(claimed))
	current, err = q.GetStoragePool(ctx, rows[0].ID)
	if err != nil || current.State != "ready" || len(current.NodeIds) != 3 {
		t.Fatal("pool expansion did not complete", err)
	}
	before := joined
	put("c", &device)
	if err = service.ConfigureManagedStorage(ctx, nil); err != nil {
		t.Fatal(err)
	}
	claimed, err = q.ClaimOperation(ctx, &owner)
	if err != nil {
		t.Fatal("existing compute member's disk selection was omitted", err)
	}
	worker.execute(ctx, queries.Operation(claimed))
	if joined != before+1 {
		t.Fatal("unchanged storage members were reconfigured")
	}
	replicas := 2
	if err = service.ConfigureManagedStorage(ctx, &replicas); err != nil {
		t.Fatal(err)
	}
	claimed, err = q.ClaimOperation(ctx, &owner)
	if err != nil {
		t.Fatal(err)
	}
	var configuration Payload
	if err = json.Unmarshal(claimed.Payload, &configuration); err != nil {
		t.Fatal(err)
	}
	if configuration.CephReplicas == nil || *configuration.CephReplicas != 2 || len(configuration.CephJoinNodes) != 0 {
		t.Fatal("replica change replayed node registration")
	}
	worker.execute(ctx, queries.Operation(claimed))
	if err = service.ConfigureManagedStorage(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = q.ClaimOperation(ctx, &owner); err != pgx.ErrNoRows {
		t.Fatal("unchanged desired storage created another operation", err)
	}
	replicas = 3
	if err = service.ConfigureManagedStorage(ctx, &replicas); err == nil {
		t.Fatal("replicas exceeded storage host count")
	}
}
