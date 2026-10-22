package metrics

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsStorage(t *testing.T) {
	var imported string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/import/prometheus" {
			body, _ := io.ReadAll(r.Body)
			imported = string(body)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/api/v1/query_range" {
			t.Error(r.URL.Path)
		}
		query := r.URL.Query().Get("query")
		if !strings.Contains(query, "environment=\"env\"") || !strings.Contains(query, "asset=\"a\\\"b\"") || !strings.Contains(query, "rate(netlab_cpu_seconds_total") || !strings.Contains(query, "timestamp(") {
			t.Error(query)
		}
		fmt.Fprint(w, "{\"status\":\"success\",\"data\":{\"resultType\":\"matrix\",\"result\":[{\"metric\":{\"metric\":\"cpu\",\"asset\":\"a\\\"b\",\"instance\":\"one\",\"node\":\"worker\"},\"values\":[[1700000000,\"0.5\"],[1700000010,\"NaN\"]]}]}}")
	}))
	defer server.Close()
	store, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	if err = Write(&data, []Sample{{Name: "cpu_seconds_total", Environment: "env", Asset: "a\"b", Instance: "one", Node: "worker", Value: 2}}, time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
	if err = store.Import(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(imported, "asset=\"a\\\"b\"") || !strings.HasSuffix(imported, " 2 1700000000000\n") {
		t.Fatal(imported)
	}
	history, err := store.History(context.Background(), Filter{Environment: "env", Asset: "a\"b"}, time.Unix(1700000000, 0), time.Unix(1700000030, 0), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Series) != 1 || len(history.Series[0].Points) != 1 || history.Series[0].Points[0].Value != 0.5 {
		t.Fatalf("unexpected history: %+v", history)
	}
}

func TestMetricsStorageFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "disk full", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	store, _ := New(server.URL)
	if err := store.Import(context.Background(), strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatal(err)
	}
	if _, err := store.History(context.Background(), Filter{Environment: "env"}, time.Now().Add(-time.Minute), time.Now(), 10); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatal(err)
	}
}

func TestHistoryScopes(t *testing.T) {
	for _, test := range []struct {
		name   string
		filter Filter
		want   []string
		absent []string
	}{
		{"environment", Filter{Environment: "lab", Aggregate: true}, []string{`sum(rate(netlab_cpu_seconds_total{environment="lab"}[1m])`, `sum(last_over_time(netlab_memory_bytes`}, []string{"cpu_capacity"}},
		{"node", Filter{Node: "worker"}, []string{`environment="",node="worker"`, "netlab_memory_available_bytes", "netlab_disk_used_bytes", "netlab_cpu_capacity"}, []string{"sum("}},
		{"cluster", Filter{Aggregate: true}, []string{`sum(rate(netlab_cpu_seconds_total{environment=""}[1m])`, `sum(last_over_time(netlab_cpu_capacity`}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			query := historyQuery(test.filter)
			for _, term := range test.want {
				if !strings.Contains(query, term) {
					t.Errorf("missing %q in %s", term, query)
				}
			}
			for _, term := range test.absent {
				if strings.Contains(query, term) {
					t.Errorf("unexpected %q in %s", term, query)
				}
			}
		})
	}
}
