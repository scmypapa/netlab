package server

import (
	"encoding/json"
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
	ctx := r.Context()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	node, err := q.LockNode(ctx, r.PathValue("id"))
	if err != nil {
		return err
	}
	if input.MemoryReserveMiB != nil {
		var info api.NodeInfo
		if err = json.Unmarshal(node.Info, &info); err != nil {
			return err
		}
		if *input.MemoryReserveMiB < 0 || *input.MemoryReserveMiB > 0 && *input.MemoryReserveMiB >= info.Capacity.MemoryMiB {
			return httpError{http.StatusBadRequest, "宿主预留内存应小于节点物理内存"}
		}
	}
	if _, err = q.SetNodeScheduling(ctx, queries.SetNodeSchedulingParams{ID: node.ID, CpuAllocationRatio: input.CpuAllocationRatio, MemoryReserveMib: input.MemoryReserveMiB}); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
