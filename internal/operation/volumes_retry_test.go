package operation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

func TestVolumeRetryBeforeFirstPhase(t *testing.T) {
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
	schema := pgx.Identifier{"volume_retry_" + strings.ReplaceAll(uuid.NewString(), "-", "")}.Sanitize()
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
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input api.NodeVolume
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.SizeGiB != 2 {
			t.Errorf("lost volume input: %+v %v", input, err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()
	if err = q.PutNode(ctx, queries.PutNodeParams{ID: "node", Name: "node", Endpoint: endpoint.URL, Info: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	worker := Worker{Pool: pool, Queries: q, Client: &transport.Client{HTTP: endpoint.Client()}}
	service := Service{Pool: pool, Queries: q}
	owner := "worker"
	for _, kind := range []string{"create-volume", "resize-volume", "delete-volume"} {
		t.Run(kind, func(t *testing.T) {
			id, opID := uuid.NewString(), uuid.NewString()
			input := api.NodeVolume{Id: id, Kind: api.Vm, SizeGiB: 2}
			raw, _ := json.Marshal(Payload{Volume: &input, VolumeNode: "node"})
			if _, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: opID, ScopeKind: "volume", ScopeID: id, Kind: kind, Payload: raw}); err != nil {
				t.Fatal(err)
			}
			if err := q.CreatePersistentVolume(ctx, queries.CreatePersistentVolumeParams{ID: id, NodeID: "node", StoragePoolID: "default:node", Name: "Volume", Kind: "vm", SizeGib: 1, OperationID: &opID}); err != nil {
				t.Fatal(err)
			}
			claimed, err := q.ClaimOperation(ctx, &owner)
			if err != nil {
				t.Fatal(err)
			}
			// Fail only the node lookup, before the task persists its first phase.
			if _, err := pool.Exec(ctx, "ALTER TABLE nodes RENAME TO hidden_nodes"); err != nil {
				t.Fatal(err)
			}
			worker.execute(ctx, queries.Operation(claimed))
			if _, err := pool.Exec(ctx, "ALTER TABLE hidden_nodes RENAME TO nodes"); err != nil {
				t.Fatal(err)
			}
			failed, err := q.GetOperation(ctx, opID)
			if err != nil || failed.State != "failed" || failed.Phase != "queued" {
				t.Fatalf("wrong failure: %+v %v", failed, err)
			}
			if _, err = service.Retry(ctx, access.Identity{Principal: queries.Principal{Kind: "user", Administrator: true}}, opID); err != nil {
				t.Fatal(err)
			}
			claimed, err = q.ClaimOperation(ctx, &owner)
			if err != nil {
				t.Fatal(err)
			}
			worker.execute(ctx, queries.Operation(claimed))
			result, err := q.GetOperation(ctx, opID)
			if err != nil || result.State != "succeeded" {
				t.Fatalf("retry failed: %+v %v", result, err)
			}
			volume, err := q.GetPersistentVolume(ctx, id)
			if kind == "delete-volume" {
				if err != pgx.ErrNoRows {
					t.Fatalf("deleted volume remains: %+v %v", volume, err)
				}
			} else if err != nil || volume.State != "ready" || volume.SizeGib != 2 {
				t.Fatalf("wrong volume fact: %+v %v", volume, err)
			}
		})
	}
}
