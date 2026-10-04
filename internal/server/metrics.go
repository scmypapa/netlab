package server

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"netlab.local/core/internal/access"
)

func metricRange(query url.Values, now time.Time) (time.Time, time.Time, int, error) {
	end := now.UTC()
	var err error
	if value := query.Get("end"); value != "" {
		end, err = time.Parse(time.RFC3339, value)
		if err != nil {
			return time.Time{}, end, 0, fmt.Errorf("end 应为 RFC3339 时间")
		}
	}
	window := 900
	if value := query.Get("range"); value != "" {
		window, err = strconv.Atoi(value)
		if err != nil || window < 10 || window > 30*24*3600 {
			return time.Time{}, end, 0, fmt.Errorf("range 应在 10 秒到 30 天之间")
		}
	}
	start := end.Add(-time.Duration(window) * time.Second)
	if value := query.Get("start"); value != "" {
		start, err = time.Parse(time.RFC3339, value)
		if err != nil {
			return start, end, 0, fmt.Errorf("start 应为 RFC3339 时间")
		}
	}
	duration := end.Sub(start)
	if duration <= 0 || duration > 30*24*time.Hour || end.After(now.Add(time.Second)) {
		return start, end, 0, fmt.Errorf("查询跨度应大于零且不超过 30 天，结束时间不能在未来")
	}
	step := max(10, int(math.Ceil(duration.Seconds()/600)))
	if value := query.Get("step"); value != "" {
		step, err = strconv.Atoi(value)
		if err != nil || step < 10 || duration.Seconds()/float64(step) > 2000 {
			return start, end, step, fmt.Errorf("step 至少 10 秒，单次最多 2000 个时间点")
		}
	}
	return start.UTC(), end.UTC(), step, nil
}

func (s *Server) environmentMetrics(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	id, asset := r.PathValue("id"), r.URL.Query().Get("assetId")
	if _, err := s.Environments.Authorized(r.Context(), identity, id, "observe", asset); err != nil {
		return err
	}
	start, end, step, err := metricRange(r.URL.Query(), time.Now())
	if err != nil {
		return httpError{http.StatusBadRequest, err.Error()}
	}
	result, err := s.Metrics.History(r.Context(), id, asset, start, end, step)
	if err != nil {
		return httpError{http.StatusBadGateway, err.Error()}
	}
	w.Header().Set("Cache-Control", "no-store")
	return writeJSON(w, http.StatusOK, result)
}
