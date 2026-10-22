//go:build linux

package engine

import (
	"encoding/json"
	"net/http"
	"time"

	"netlab.local/core/api"
)

func (e *Engine) Traffic(w http.ResponseWriter, r *http.Request) {
	if e.trafficError != nil {
		http.Error(w, e.trafficError.Error(), http.StatusServiceUnavailable)
		return
	}
	var interfaces []api.CaptureInterface
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&interfaces); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	value, err := e.collector.Snapshot(r.Context(), interfaces, time.Now().UTC())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}
