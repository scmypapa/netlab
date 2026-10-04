//go:build linux

package engine

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"

	"netlab.local/core/api"
)

func (e *Engine) Capture(w http.ResponseWriter, r *http.Request) {
	if e.captureError != nil {
		http.Error(w, e.captureError.Error(), http.StatusServiceUnavailable)
		return
	}
	environment, id := r.PathValue("environmentId"), r.PathValue("captureId")
	var value any
	var err error
	switch {
	case r.Method == http.MethodGet && id == "":
		value, err = e.captures.List(environment)
	case r.Method == http.MethodDelete && id == "":
		err = e.captures.RemoveEnvironment(r.Context(), environment)
	case r.Method == http.MethodPost && id == "":
		unlock := e.lock(environment)
		defer unlock()
		var request api.NodeCaptureRequest
		err = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&request)
		if err == nil && request.EnvironmentId != environment {
			http.Error(w, "环境标识不一致", http.StatusBadRequest)
			return
		}
		if err == nil {
			value, err = e.captures.Start(r.Context(), request)
		}
	case r.Method == http.MethodGet && r.PathValue("file") != "":
		err = e.captures.File(w, r, id, environment)
		if err == nil {
			return
		}
	case r.Method == http.MethodGet:
		value, err = e.captures.Get(id, environment)
	case r.Method == http.MethodPost:
		value, err = e.captures.Stop(r.Context(), id, environment)
	case r.Method == http.MethodDelete:
		err = e.captures.Remove(r.Context(), id, environment)
	}
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	if value == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}
