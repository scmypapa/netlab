//go:build linux

package engine

import (
	"encoding/json"
	"net/http"

	"netlab.local/core/api"
)

func (e *Engine) Security(w http.ResponseWriter, r *http.Request) {
	var input api.NodeSecurityRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&input); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	retention := map[string]api.PacketRetention{}
	if e.captureError == nil && input.PacketQuotas != nil {
		var err error
		retention, err = e.captures.Reconcile(r.Context(), input.Interfaces, *input.PacketQuotas)
		if err != nil {
			retention = e.captures.Retention()
			for environment, info := range retention {
				if _, exists := (*input.PacketQuotas)[environment]; !exists {
					delete(retention, environment)
					continue
				}
				message := err.Error()
				info.Status, info.Error = api.PacketRetentionStatusUnavailable, &message
				retention[environment] = info
			}
		}
	}
	if e.securityError != nil {
		message := e.securityError.Error()
		json.NewEncoder(w).Encode(api.NodeSecurityObservation{Cursor: input.Cursor, Alerts: []api.SecurityAlert{}, Sensor: api.SecuritySensor{NodeId: e.cfg.ID, Status: "unavailable", Error: &message}, Coverage: map[string]int{}, PacketRetention: &retention})
		return
	}
	result, err := e.security.Poll(r.Context(), input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	result.PacketRetention = &retention
	json.NewEncoder(w).Encode(result)
}
