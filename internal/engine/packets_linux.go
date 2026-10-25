//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"netlab.local/core/internal/capture"
	"os"
	"time"
)

func (e *Engine) Packets(w http.ResponseWriter, r *http.Request) {
	if e.captureError != nil {
		http.Error(w, e.captureError.Error(), http.StatusServiceUnavailable)
		return
	}
	query, err := capture.ParseRecentQuery(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	environment := r.PathValue("environmentId")
	if r.PathValue("packetId") == "export" {
		w.Header().Set("Content-Type", "application/octet-stream")
		output := &capture.PacketStream{Writer: w}
		if err = e.captures.ExportRecent(r.Context(), environment, query, output); err != nil {
			if output.Bytes > 0 {
				panic(http.ErrAbortHandler)
			}
			status := http.StatusConflict
			if errors.Is(err, capture.ErrAnalysisBusy) {
				status = http.StatusServiceUnavailable
			}
			http.Error(w, err.Error(), status)
		}
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var value any
	if id := r.PathValue("packetId"); id != "" {
		value, err = e.captures.RecentPacket(ctx, environment, id)
	} else {
		value, err = e.captures.Recent(ctx, environment, query)
	}
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusGone
		} else if errors.Is(err, capture.ErrAnalysisBusy) {
			status = http.StatusServiceUnavailable
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}
