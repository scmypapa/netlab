package server

import (
	"math"
	"net/http"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
)

func (s *Server) configureNodeScheduling(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	var input api.NodeScheduling
	if err := decode(w, r, &input); err != nil {
		return err
	}
	if math.IsNaN(input.CpuAllocationRatio) || math.IsInf(input.CpuAllocationRatio, 0) || input.CpuAllocationRatio < 1 {
		return httpError{http.StatusBadRequest, "CPU 分配比例应至少为 1"}
	}
	count, err := s.Queries.SetNodeScheduling(r.Context(), queries.SetNodeSchedulingParams{ID: r.PathValue("id"), CpuAllocationRatio: input.CpuAllocationRatio})
	if err != nil {
		return err
	}
	if count == 0 {
		return httpError{http.StatusNotFound, "节点不存在"}
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
