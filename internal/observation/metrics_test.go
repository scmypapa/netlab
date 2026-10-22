package observation

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

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
