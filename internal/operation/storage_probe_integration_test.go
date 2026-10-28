package operation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/transport"
)

func TestStoragePlanningSkipsOfflineNodeProbes(t *testing.T) {
	pool := concurrencyPool(t, 4)
	var offlineRequests atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/offline/node/v1/info" {
			offlineRequests.Add(1)
			<-r.Context().Done()
			return
		}
		json.NewEncoder(w).Encode(api.NodeInfo{Storage: &api.StorageInfo{Path: "/data", Filesystem: "disk", AvailableBytes: 10 << 30, CapacityBytes: 20 << 30}})
	}))
	defer endpoint.Close()
	worker := Worker{Pool: pool, Queries: queries.New(pool), Client: &transport.Client{HTTP: endpoint.Client()}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	storage, err := worker.storage(ctx, []queries.ListNodesRow{
		{ID: "online", State: "ready", Endpoint: endpoint.URL},
		{ID: "offline", State: "offline", Endpoint: endpoint.URL + "/offline"},
		{ID: "retiring", State: "ready", Retiring: true, Endpoint: endpoint.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	if offlineRequests.Load() != 0 {
		t.Fatalf("scheduling waited on %d probes to a known offline node", offlineRequests.Load())
	}
	if candidate := storage[storageKey("online", defaultStorage("online"))]; !candidate.ready || candidate.info.CapacityBytes != 20<<30 || candidate.err != nil {
		t.Fatalf("online storage=%+v", candidate)
	}
	if candidate := storage[storageKey("retiring", defaultStorage("retiring"))]; candidate.ready || candidate.info.CapacityBytes != 20<<30 || candidate.err != nil {
		t.Fatalf("retiring node must retain storage metadata for volume removal: %+v", candidate)
	}
	if candidate := storage[storageKey("offline", defaultStorage("offline"))]; candidate.ready || candidate.err == nil {
		t.Fatalf("offline node must remain unavailable: %+v", candidate)
	}
}
