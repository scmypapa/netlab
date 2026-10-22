package observation

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"netlab.local/core/internal/metrics"
	"netlab.local/core/internal/transport"
)

func TestPartialMetricsAreImportedBeforeReportingError(t *testing.T) {
	const sample = "netlab_memory_available_bytes{node=\"worker\"} 1024 1700000000000\n"
	const failure = "容器采集失败\nconnection refused"
	var imported string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		imported = string(data)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer storage.Close()
	store, err := metrics.New(storage.URL)
	if err != nil {
		t.Fatal(err)
	}
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(metrics.CollectionErrorHeader, strconv.QuoteToASCII(failure))
		io.WriteString(w, sample)
	}))
	defer node.Close()
	service := Service{Client: &transport.Client{HTTP: node.Client()}, Metrics: store}
	err = service.nodeMetrics(context.Background(), node.URL)
	if err == nil || !strings.Contains(err.Error(), failure) {
		t.Fatalf("partial metrics error was lost: %v", err)
	}
	if imported != sample {
		t.Fatalf("successful host samples were not imported: %q", imported)
	}
}

func TestMetricImportsDoNotWaitForAnotherNode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	imported := make(chan struct{}, 1)
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		imported <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer storage.Close()
	store, _ := metrics.New(storage.URL)
	slowStarted := make(chan struct{})
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/slow/") {
			close(slowStarted)
			<-r.Context().Done()
			return
		}
		io.WriteString(w, "netlab_cpu_capacity{node=\"fast\"} 16\n")
	}))
	defer node.Close()
	service := Service{Client: &transport.Client{HTTP: node.Client()}, Metrics: store}
	finished := make(chan error, 2)
	go func() { finished <- service.nodeMetrics(ctx, node.URL+"/slow") }()
	<-slowStarted
	go func() { finished <- service.nodeMetrics(ctx, node.URL+"/fast") }()
	select {
	case <-imported:
	case <-time.After(time.Second):
		t.Fatal("healthy node import waited for a slow node")
	}
	cancel()
	for range 2 {
		<-finished
	}
}

func TestConcurrentMixedMetricImports(t *testing.T) {
	const nodes, assets = 64, 64
	var lock sync.Mutex
	imported := map[string]int{}
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		text := string(data)
		first := strings.Index(text, `node="`)
		if first < 0 {
			t.Error("import did not retain node identity")
			http.Error(w, "missing node", http.StatusBadRequest)
			return
		}
		id := strings.SplitN(text[first+6:], `"`, 2)[0]
		lock.Lock()
		imported[id] += strings.Count(text, "\n")
		lock.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer storage.Close()
	store, _ := metrics.New(storage.URL)
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")[0]
		if id == "node-0" {
			http.Error(w, "collector unavailable", http.StatusServiceUnavailable)
			return
		}
		if id == "node-1" {
			w.Header().Set(metrics.CollectionErrorHeader, strconv.QuoteToASCII("VM collector unavailable"))
		}
		fmt.Fprintf(w, "netlab_memory_bytes{node=%q} 1024\n", id)
		if id == "node-1" {
			return
		}
		for asset := range assets {
			kind := "container"
			if asset%2 != 0 {
				kind = "vm"
			}
			fmt.Fprintf(w, "netlab_cpu_seconds_total{node=%q,environment=\"lab\",asset=\"%s-%d\"} 10\n", id, kind, asset)
			fmt.Fprintf(w, "netlab_memory_bytes{node=%q,environment=\"lab\",asset=\"%s-%d\"} 1024\n", id, kind, asset)
		}
	}))
	defer node.Close()
	service := Service{Client: &transport.Client{HTTP: node.Client()}, Metrics: store}
	var waiting sync.WaitGroup
	failures := make(chan string, nodes)
	for number := range nodes {
		waiting.Go(func() {
			id := fmt.Sprintf("node-%d", number)
			if err := service.nodeMetrics(context.Background(), node.URL+"/"+id); err != nil {
				failures <- id
			}
		})
	}
	waiting.Wait()
	close(failures)
	failed := map[string]bool{}
	for id := range failures {
		failed[id] = true
	}
	if len(failed) != 2 || !failed["node-0"] || !failed["node-1"] {
		t.Fatal("a failed collector affected other nodes")
	}
	if len(imported) != nodes-1 || imported["node-1"] != 1 {
		t.Fatalf("host-only imports were lost: %v", imported)
	}
	for number := 2; number < nodes; number++ {
		id := fmt.Sprintf("node-%d", number)
		if imported[id] != 1+assets*2 {
			t.Errorf("%s imported %d samples; want %d", id, imported[id], 1+assets*2)
		}
	}
}
