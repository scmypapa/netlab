package server

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/environment"
	"netlab.local/core/internal/operation"
)

func (s *Server) templateCache(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	ctx, id := r.Context(), r.PathValue("id")
	rows, err := s.Queries.GetTemplates(ctx, []string{id})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return environment.Invalid("模板不存在")
	}
	var template api.Template
	if err = json.Unmarshal(rows[0].Definition, &template); err != nil {
		return err
	}
	if r.Method == http.MethodDelete {
		tx, err := s.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		q := s.Queries.WithTx(tx)
		row, err := q.LockTemplate(ctx, id)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(row.Definition, &template); err != nil {
			return err
		}
		if template.Kind != api.Vm || template.State == nil || *template.State != api.TemplateStateReady {
			return environment.Invalid("模板尚未就绪")
		}
		if template.OperationId != nil {
			latest, err := q.GetOperation(ctx, *template.OperationId)
			if err != nil {
				return err
			}
			if latest.State == "queued" || latest.State == "running" {
				return environment.ErrConflict
			}
		}
		opID := uuid.NewString()
		template.OperationId = &opID
		raw, err := json.Marshal(template)
		if err != nil {
			return err
		}
		if err = q.UpdateTemplate(ctx, queries.UpdateTemplateParams{ID: id, Definition: raw}); err != nil {
			return err
		}
		payload, err := json.Marshal(operation.Payload{Template: &template})
		if err != nil {
			return err
		}
		op, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: opID, ScopeKind: "template", ScopeID: id, Kind: "trim-template-cache", Payload: payload, ExpectedRevision: int32(template.Version)})
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
	nodes, err := s.Queries.ListNodes(ctx)
	if err != nil {
		return err
	}
	references, err := s.Queries.TemplateLocalReferences(ctx, id)
	if err != nil {
		return err
	}
	pools, err := s.Queries.ListStoragePools(ctx)
	if err != nil {
		return err
	}
	result := []api.TemplateCache{}
	for _, node := range nodes {
		if node.State != "ready" {
			continue
		}
		var cache api.TemplateCache
		if err = s.Nodes.Do(ctx, http.MethodGet, node.Endpoint, operation.TemplateCacheRoute(template), nil, &cache); err != nil {
			return err
		}
		cache.NodeName = node.Name
		shared := false
		for _, pool := range pools {
			if pool.Driver == "rbd" && pool.State == "ready" {
				for _, member := range pool.NodeIds {
					shared = shared || member == node.ID
				}
			}
		}
		if !shared {
			cache.Reclaimable = false
			cache.Reason = "未接入共享存储"
		}
		for _, reference := range references {
			if reference == node.ID {
				cache.Reclaimable = false
				cache.Reason = "本地资产或恢复点正在引用"
			}
		}
		if cache.Bytes > 0 {
			result = append(result, cache)
		}
	}
	return writeJSON(w, http.StatusOK, result)
}
