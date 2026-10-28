package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"netlab.local/core/api"
	"netlab.local/core/internal/metrics"
)

func TestResourceMetricsRejectsUnsupportedSelection(t *testing.T) {
	for _, selection := range []string{"", "unknown", "receive,,transmit", "cpu_capacity"} {
		server := &Server{}
		request := httptest.NewRequest(http.MethodGet, "/metrics?metrics="+selection, nil)
		response := httptest.NewRecorder()
		if err := server.resourceMetrics(response, request, metrics.Filter{Environment: "lab"}); err != nil {
			writeError(response, request, err)
		}
		if response.Code != http.StatusBadRequest {
			t.Fatalf("unsupported selection %q returned %d", selection, response.Code)
		}
	}
}

func TestRealResourceMetricSelectionHTTP(t *testing.T) {
	endpoint := os.Getenv("NETLAB_REAL_METRICS_URL")
	if endpoint == "" {
		t.Skip("set NETLAB_REAL_METRICS_URL to an isolated VictoriaMetrics instance")
	}
	store, err := metrics.New(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	environment := "selection-" + uuid.NewString()
	base := time.Now().UTC().Truncate(10 * time.Second).Add(-2 * time.Minute)
	var source bytes.Buffer
	for index := range 4 {
		for name, value := range map[string]float64{
			"cpu_seconds_total":              float64(index) * 0.5,
			"memory_bytes":                   128 << 20,
			"interface_receive_bytes_total":  float64(index * 4096),
			"interface_transmit_bytes_total": float64(index * 2048),
		} {
			sample := metrics.Sample{Name: name, Value: value, Environment: environment, Asset: "server", Instance: "instance", Node: "node"}
			if strings.HasPrefix(name, "interface_") {
				sample.Interface = "nic"
			}
			if err := metrics.Write(&source, []metrics.Sample{sample}, base.Add(time.Duration(index)*10*time.Second)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.Import(context.Background(), &source); err != nil {
		t.Fatal(err)
	}
	flushed, err := store.HTTP.Post(endpoint+"/internal/force_flush", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	flushed.Body.Close()
	if flushed.StatusCode != http.StatusOK {
		t.Fatalf("fixture flush returned HTTP %d", flushed.StatusCode)
	}
	server := &Server{Metrics: store}
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := server.resourceMetrics(w, r, metrics.Filter{Environment: environment}); err != nil {
			writeError(w, r, err)
		}
	}))
	defer httpServer.Close()
	for _, test := range []struct {
		selection string
		want      []string
	}{
		{"", []string{"cpu", "memory", "receive", "transmit"}},
		{"receive,transmit", []string{"receive", "transmit"}},
	} {
		query := url.Values{"start": {base.Add(10 * time.Second).Format(time.RFC3339)}, "end": {base.Add(30 * time.Second).Format(time.RFC3339)}, "step": {"10"}}
		if test.selection != "" {
			query.Set("metrics", test.selection)
		}
		response, err := httpServer.Client().Get(httpServer.URL + "/metrics?" + query.Encode())
		if err != nil {
			t.Fatal(err)
		}
		var history api.MetricHistory
		err = json.NewDecoder(response.Body).Decode(&history)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("metric selection %q returned HTTP %d: %v", test.selection, response.StatusCode, err)
		}
		found := make([]string, 0, len(history.Series))
		for _, series := range history.Series {
			found = append(found, string(series.Metric))
			if len(series.Points) == 0 {
				t.Fatalf("metric %s contained no measured values", series.Metric)
			}
		}
		slices.Sort(found)
		if !slices.Equal(found, test.want) {
			t.Fatalf("selection %q returned %v, want %v", test.selection, found, test.want)
		}
	}
}
