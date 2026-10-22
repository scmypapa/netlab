//go:build linux

package engine

import (
	"encoding/json"
	"net/http"

	"netlab.local/core/api"
)

func (e *Engine) Security(w http.ResponseWriter, r *http.Request) {
	if e.securityError != nil {
		http.Error(w, e.securityError.Error(), http.StatusServiceUnavailable)
		return
	}
	var input api.NodeSecurityRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&input); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	result, err := e.security.Poll(r.Context(), input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}
