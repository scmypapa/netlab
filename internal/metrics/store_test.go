package metrics

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"netlab.local/core/api"
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
			query := historyQuery(test.filter, 10)
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

func TestHistoryBucketQuery(t *testing.T) {
	query := historyQuery(Filter{Environment: "lab", Aggregate: true}, 4320)
	if !strings.Contains(query, `sum(increase(netlab_cpu_seconds_total{environment="lab"}[4320s]) / 4320)`) ||
		!strings.Contains(query, `sum(increase(netlab_interface_transmit_bytes_total{environment="lab"}[4320s]) / 4320)`) {
		t.Fatal(query)
	}
	if strings.Contains(query, `timestamp(netlab_cpu_seconds_total`) || !strings.Contains(query, `last_over_time(netlab_memory_bytes{environment="lab"}[15s])`) {
		t.Fatal("long counters lost bucket activity or changed point-in-time memory semantics:", query)
	}
}

func TestRealMetricsHistory(t *testing.T) {
	endpoint := os.Getenv("NETLAB_REAL_METRICS_URL")
	if endpoint == "" {
		t.Skip("set NETLAB_REAL_METRICS_URL to an isolated VictoriaMetrics instance")
	}
	store, err := New(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	prefix := fmt.Sprintf("metrics-test-%d", time.Now().UnixNano())
	base := time.Now().UTC().Truncate(10 * time.Minute).Add(-time.Hour)
	var data bytes.Buffer
	write := func(sample Sample, seconds int) {
		t.Helper()
		if err := Write(&data, []Sample{sample}, base.Add(time.Duration(seconds)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	curves := map[string][]struct{ second, counter int }{
		"burst":   {{0, 0}, {120, 0}, {180, 600}, {600, 600}, {1200, 600}},
		"reset":   {{0, 0}, {120, 120}, {180, 0}, {300, 120}, {600, 420}, {1200, 420}},
		"stopped": {{0, 0}, {60, 60}, {120, 120}},
		"absent":  {{-1200, 0}, {-1140, 60}},
	}
	for asset, points := range curves {
		for _, point := range points {
			write(Sample{Name: "cpu_seconds_total", Environment: prefix, Asset: asset, Instance: asset, Node: prefix, Value: float64(point.counter)}, point.second)
			write(Sample{Name: "interface_transmit_bytes_total", Environment: prefix, Asset: asset, Instance: asset, Node: prefix, Interface: "eth0", Value: float64(point.counter) * 1024}, point.second)
		}
	}
	const nodes, environmentsPerNode, assetsPerEnvironment = 8, 4, 32
	for node := range nodes {
		id := fmt.Sprintf("%s-node-%d", prefix, node)
		for point := range 4 {
			at := 1500 + point*10
			for _, sample := range []Sample{
				{Name: "cpu_seconds_total", Node: id, Value: float64(point+1) * 240},
				{Name: "cpu_capacity", Node: id, Value: 32},
				{Name: "memory_bytes", Node: id, Value: 16 << 30},
				{Name: "memory_capacity_bytes", Node: id, Value: 32 << 30},
			} {
				write(sample, at)
			}
			for lab := range environmentsPerNode {
				for asset := range assetsPerEnvironment {
					kind, cpu, memory, traffic := "container", 0.1, float64(64<<20), float64(512)
					if asset%2 != 0 {
						kind, cpu, memory, traffic = "vm", 0.2, 128<<20, 1024
					}
					identity := Sample{Environment: fmt.Sprintf("%s-lab-%d-%d", prefix, node, lab), Asset: fmt.Sprintf("%s-%d", kind, asset), Instance: fmt.Sprintf("instance-%d", asset), Node: id}
					for name, value := range map[string]float64{
						"cpu_seconds_total":              cpu * float64((point+1)*10),
						"memory_bytes":                   memory,
						"disk_read_bytes_total":          float64((point + 1) * 40960),
						"disk_write_bytes_total":         float64((point + 1) * 20480),
						"interface_receive_bytes_total":  traffic * float64((point+1)*10),
						"interface_transmit_bytes_total": traffic * float64((point+1)*10),
					} {
						sample := identity
						sample.Name, sample.Value = name, value
						if strings.HasPrefix(name, "interface_") {
							sample.Interface = "eth0"
						}
						write(sample, at)
					}
				}
			}
		}
	}
	if err := store.Import(ctx, &data); err != nil {
		t.Fatal(err)
	}
	// Publish the complete fixture before checking historical query semantics.
	flush, err := store.HTTP.Post(endpoint+"/internal/force_flush", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	flush.Body.Close()
	if flush.StatusCode != http.StatusOK {
		t.Fatalf("fixture flush returned HTTP %d", flush.StatusCode)
	}
	read := func(filter Filter, start, end time.Time, step int) api.MetricHistory {
		t.Helper()
		history, err := store.History(ctx, filter, start, end, step)
		if err != nil {
			t.Fatal(err)
		}
		return history
	}
	value := func(history api.MetricHistory, metric string, at time.Time) (float64, bool) {
		for _, series := range history.Series {
			if string(series.Metric) == metric {
				for _, point := range series.Points {
					if point.Time.Equal(at) {
						return point.Value, true
					}
				}
			}
		}
		return 0, false
	}
	first := base.Add(600 * time.Second)
	for asset, cumulative := range map[string]float64{"burst": 600, "reset": 540, "stopped": 120} {
		t.Run(asset, func(t *testing.T) {
			history := read(Filter{Environment: prefix, Asset: asset}, first, base.Add(1200*time.Second), 600)
			for metric, scale := range map[string]float64{"cpu": 1, "transmit": 1024} {
				got, found := value(history, metric, first)
				if !found || math.Abs(got*600-cumulative*scale) > 1e-5 {
					t.Errorf("%s first bucket integrated to %v; want %v (found=%v)", metric, got*600, cumulative*scale, found)
				}
			}
			if asset == "stopped" {
				if _, found := value(history, "cpu", base.Add(1200*time.Second)); found {
					t.Error("a window without observations was turned into measured idle CPU")
				}
			}
		})
	}
	if history := read(Filter{Environment: prefix, Asset: "absent"}, first, first, 600); len(history.Series) != 0 {
		t.Errorf("old samples leaked into an empty bucket: %+v", history)
	}
	end := base.Add(1530 * time.Second)
	for _, test := range []struct {
		name   string
		filter Filter
		want   map[string]float64
	}{
		{"environment", Filter{Environment: prefix + "-lab-0-0", Aggregate: true}, map[string]float64{"cpu": 4.8, "memory": 3 << 30, "receive": 24576}},
		{"container", Filter{Environment: prefix + "-lab-0-0", Asset: "container-0"}, map[string]float64{"cpu": 0.1, "memory": 64 << 20, "receive": 512}},
		{"vm", Filter{Environment: prefix + "-lab-0-0", Asset: "vm-1"}, map[string]float64{"cpu": 0.2, "memory": 128 << 20, "receive": 1024}},
		{"node", Filter{Node: prefix + "-node-0", Aggregate: true}, map[string]float64{"cpu": 24, "memory": 16 << 30, "cpu_capacity": 32}},
		{"cluster", Filter{Aggregate: true}, map[string]float64{"cpu": 192, "memory": 128 << 30, "cpu_capacity": 256}},
	} {
		t.Run(test.name, func(t *testing.T) {
			history := read(test.filter, end.Add(-30*time.Second), end, 10)
			for metric, want := range test.want {
				got, found := value(history, metric, end)
				if !found || math.Abs(got-want) > 1e-5 {
					t.Errorf("%s=%v, want %v (found=%v)", metric, got, want, found)
				}
			}
		})
	}
}
