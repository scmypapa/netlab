package server

import (
	"context"
	"net/http"
	"time"

	"netlab.local/core/api"
	"netlab.local/core/internal/access"
)

func (s *Server) discoverExternalDevices(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	var input api.ExternalDiscovery
	if err := decode(w, r, &input); err != nil {
		return err
	}
	nodes, err := s.Queries.GetNodeEndpoints(r.Context(), []string{r.PathValue("id")})
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return httpError{http.StatusNotFound, "节点不存在"}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	var result []api.DiscoveredDevice
	if err := s.Nodes.Do(ctx, http.MethodPost, nodes[0].Endpoint, "/node/v1/external-devices", input, &result); err != nil {
		return httpError{http.StatusUnprocessableEntity, err.Error()}
	}
	return writeJSON(w, http.StatusOK, result)
}
