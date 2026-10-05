package server

import (
	"net/http"

	"netlab.local/core/api"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/operation"
)

func (s *Server) assetMigration(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	service := operation.Service{Pool: s.Pool, Queries: s.Queries}
	id, asset := r.PathValue("id"), r.PathValue("assetId")
	if r.Method == http.MethodGet {
		result, err := service.MigrationDestinations(r.Context(), identity, id, asset)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, result)
	}
	var input api.MigrationRequest
	if err := decode(w, r, &input); err != nil {
		return err
	}
	result, err := service.Migrate(r.Context(), identity, id, asset, input)
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/api/v1/operations/"+result.Id)
	return writeJSON(w, http.StatusAccepted, result)
}
