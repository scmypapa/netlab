package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/environment"
	"netlab.local/core/internal/operation"
)

func (s *Server) nodeRetirement(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	ctx, id := r.Context(), r.PathValue("id")
	worker := operation.Worker{Pool: s.Pool, Queries: s.Queries, Client: s.Nodes, Secrets: s.Secrets}
	if r.Method == http.MethodGet {
		plan, err := worker.NodeRetirement(ctx, id)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, plan)
	}
	if r.Method == http.MethodPost {
		plan, err := worker.NodeRetirement(ctx, id)
		if err != nil {
			return err
		}
		if len(plan.Blockers) > 0 {
			return environment.Invalid("%s", plan.Blockers[0])
		}
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	node, err := q.LockNode(ctx, id)
	if err != nil {
		return err
	}
	latest, err := q.NodeOperation(ctx, id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if latest.State == "running" || latest.State == "queued" && r.Method != http.MethodDelete {
		return environment.ErrConflict
	}
	if r.Method == http.MethodDelete {
		if latest.Kind == "retire-node" && (latest.Phase == "retire-storage" || latest.Phase == "retire-storage-cleanup" || latest.Phase == "retire-complete") {
			return environment.Invalid("存储退出已开始，请继续完成节点退出")
		}
		if err = q.CancelNodeRetirement(ctx, id); err != nil {
			return err
		}
		if err = q.ResumeNode(ctx, id); err != nil {
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	if node.State != "ready" || node.Retiring {
		return environment.Invalid("节点尚未恢复调度")
	}
	blockers, err := q.NodeDependencies(ctx, id)
	if err != nil {
		return err
	}
	if len(blockers) > 0 {
		return environment.Invalid("%s", blockers[0])
	}
	if err = q.DrainNode(ctx, id); err != nil {
		return err
	}
	raw, err := json.Marshal(operation.Payload{})
	if err != nil {
		return err
	}
	op, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: uuid.NewString(), ScopeKind: "node", ScopeID: id, Kind: "retire-node", Payload: raw})
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	result, err := environment.Operation(op)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, result)
}
