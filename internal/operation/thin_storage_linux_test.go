//go:build linux

package operation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/engine"
	"netlab.local/core/internal/transport"
)

func TestThinVolumeAdmissionSerializesConcurrentWrites(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img is required")
	}
	pool := concurrencyPool(t, 12)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	q := queries.New(pool)
	// Two GiB remain after the filesystem reserve; each sparse volume needs less than one GiB.
	info := api.NodeInfo{Id: "node", Capabilities: []string{"vm", "thin-storage"}, Storage: &api.StorageInfo{Path: t.TempDir(), Filesystem: "disk", CapacityBytes: 20 << 30, AvailableBytes: 3 << 30}}
	var executor engine.Engine
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/node/v1/info" {
			json.NewEncoder(w).Encode(info)
			return
		}
		var input api.NodeStorageEstimate
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			http.Error(w, err.Error(), 400)
			return
		}
		parts, err := executor.EstimateStorage(r.Context(), input)
		if err != nil {
			t.Error(err)
			http.Error(w, err.Error(), 500)
			return
		}
		// The test engine has no node ID; expose its actual QEMU measurement under this node's filesystem identity.
		for key, values := range parts {
			for i := range values {
				values[i].Key = "node/disk"
			}
			parts[key] = values
		}
		json.NewEncoder(w).Encode(parts)
	}))
	defer node.Close()
	raw, _ := json.Marshal(info)
	if err := q.PutNode(ctx, queries.PutNodeParams{ID: "node", Name: "Node", Endpoint: node.URL, Info: raw}); err != nil {
		t.Fatal(err)
	}
	worker := Worker{Pool: pool, Queries: q, Client: &transport.Client{HTTP: node.Client()}}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, err := worker.SubmitVolume(ctx, uuid.NewString(), "create-volume", api.CreateVolume{Name: "Sparse", Kind: api.Vm, SizeGiB: 1024, StoragePoolId: "default:node"})
			if err == nil {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	if accepted.Load() != 2 {
		t.Fatalf("concurrent admission accepted %d volumes, want 2", accepted.Load())
	}
	observed := time.Now()
	if _, err := pool.Exec(ctx, "UPDATE operations SET phase='complete',state='succeeded',updated_at=now() WHERE kind='create-volume'"); err != nil {
		t.Fatal(err)
	}
	stale, err := q.PendingStorageWrites(ctx, pgtype.Timestamptz{Time: observed, Valid: true})
	if err != nil || len(stale) != 1 || stale[0].Gib != 2 {
		t.Fatalf("writes completed after measurement were lost: %+v %v", stale, err)
	}
	fresh, err := q.PendingStorageWrites(ctx, pgtype.Timestamptz{Time: time.Now(), Valid: true})
	if err != nil || len(fresh) != 0 {
		t.Fatalf("completed writes remained after fresh measurement: %+v %v", fresh, err)
	}
}
