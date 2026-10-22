package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"netlab.local/core/api"
)

type Store struct {
	URL  string
	HTTP *http.Client
}

func New(endpoint string) (*Store, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("invalid metrics endpoint")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 32
	return &Store{URL: strings.TrimRight(endpoint, "/"), HTTP: &http.Client{Timeout: 10 * time.Second, Transport: transport}}, nil
}

func (s *Store) Import(ctx context.Context, source io.Reader) error {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/api/v1/import/prometheus", source)
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "text/plain; version=0.0.4")
	response, err := s.HTTP.Do(r)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return responseError(response)
	}
	_, err = io.Copy(io.Discard, response.Body)
	return err
}

func responseError(response *http.Response) error {
	detail, err := io.ReadAll(io.LimitReader(response.Body, 16384))
	if err != nil {
		return err
	}
	return fmt.Errorf("metrics service returned %d: %s", response.StatusCode, strings.TrimSpace(string(detail)))
}

type Filter struct {
	Environment, Asset, Node string
	Aggregate                bool
}

type measurement struct {
	metric, source string
	rate           bool
}

var measurements = []measurement{
	{"cpu", "cpu_seconds_total", true},
	{"memory", "memory_bytes", false},
	{"disk_read", "disk_read_bytes_total", true},
	{"disk_write", "disk_write_bytes_total", true},
	{"receive", "interface_receive_bytes_total", true},
	{"transmit", "interface_transmit_bytes_total", true},
	{"receive_packets", "interface_receive_packets_total", true},
	{"transmit_packets", "interface_transmit_packets_total", true},
	{"receive_drops", "interface_receive_drops_total", true},
	{"transmit_drops", "interface_transmit_drops_total", true},
}

var hostMeasurements = []measurement{
	{"cpu_capacity", "cpu_capacity", false},
	{"memory_capacity", "memory_capacity_bytes", false},
	{"memory_available", "memory_available_bytes", false},
	{"disk_used", "disk_used_bytes", false},
	{"disk_capacity", "disk_capacity_bytes", false},
}

func historyQuery(scope Filter) string {
	filter := "environment=" + strconv.Quote(scope.Environment)
	if scope.Asset != "" {
		filter += ",asset=" + strconv.Quote(scope.Asset)
	}
	if scope.Node != "" {
		filter += ",node=" + strconv.Quote(scope.Node)
	}
	measures := measurements
	if scope.Environment == "" {
		measures = append(append([]measurement{}, measurements...), hostMeasurements...)
	}
	queries := make([]string, 0, len(measures))
	for _, measure := range measures {
		expression := "netlab_" + measure.source + "{" + filter + "}"
		if measure.rate {
			expression = "rate(" + expression + "[1m])"
		} else {
			expression = "last_over_time(" + expression + "[15s])"
		}
		// Stale counters must leave a gap, not become an apparent idle asset.
		expression += " and (time() - timestamp(netlab_" + measure.source + "{" + filter + "}) < 15)"
		if scope.Aggregate {
			expression = "sum(" + expression + ")"
		}
		queries = append(queries, "label_replace(("+expression+"),\"metric\",\""+measure.metric+"\",\"\",\".*\")")
	}
	return strings.Join(queries, " or ")
}

func (s *Store) History(ctx context.Context, filter Filter, start, end time.Time, step int) (api.MetricHistory, error) {
	result := api.MetricHistory{Start: start, End: end, StepSeconds: step, Series: []api.MetricSeries{}}
	params := url.Values{"query": {historyQuery(filter)}, "start": {strconv.FormatFloat(float64(start.UnixMilli())/1000, 'f', 3, 64)}, "end": {strconv.FormatFloat(float64(end.UnixMilli())/1000, 'f', 3, 64)}, "step": {strconv.Itoa(step)}}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+"/api/v1/query_range?"+params.Encode(), nil)
	if err != nil {
		return result, err
	}
	response, err := s.HTTP.Do(r)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, responseError(response)
	}
	var body struct {
		Status string
		Error  string
		Data   struct {
			ResultType string
			Result     []struct {
				Metric map[string]string
				Values [][2]json.RawMessage
			}
		}
	}
	if err = json.NewDecoder(response.Body).Decode(&body); err != nil {
		return result, err
	}
	if body.Status != "success" || body.Data.ResultType != "matrix" {
		return result, fmt.Errorf("metrics query failed: %s", body.Error)
	}
	for _, row := range body.Data.Result {
		series := api.MetricSeries{Metric: api.MetricSeriesMetric(row.Metric["metric"]), AssetId: row.Metric["asset"], InstanceId: row.Metric["instance"], NodeId: row.Metric["node"], Points: []api.MetricPoint{}}
		if iface := row.Metric["interface"]; iface != "" {
			series.InterfaceId = &iface
		}
		for _, pair := range row.Values {
			var timestamp float64
			var text string
			if err = json.Unmarshal(pair[0], &timestamp); err != nil {
				return result, err
			}
			if err = json.Unmarshal(pair[1], &text); err != nil {
				return result, err
			}
			value, parseErr := strconv.ParseFloat(text, 64)
			if parseErr != nil {
				return result, parseErr
			}
			if math.IsNaN(value) || math.IsInf(value, 0) {
				continue
			}
			series.Points = append(series.Points, api.MetricPoint{Time: time.UnixMilli(int64(timestamp * 1000)).UTC(), Value: value})
		}
		result.Series = append(result.Series, series)
	}
	return result, nil
}
