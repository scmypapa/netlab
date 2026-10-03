package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/environment"
	"netlab.local/core/internal/operation"
)

func (s *Server) deleteTemplate(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	ctx, id := r.Context(), r.PathValue("id")
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
	var template api.Template
	if err = json.Unmarshal(row.Definition, &template); err != nil {
		return err
	}
	if template.State != nil && (*template.State == api.TemplateStateImporting || *template.State == api.TemplateStateDeleting) {
		return httpError{http.StatusConflict, "请等待当前模板任务完成；失败任务可重试"}
	}
	references, err := q.TemplateReferences(ctx, id)
	if err != nil {
		return err
	}
	if len(references) > 0 {
		return httpError{http.StatusConflict, "模板仍被引用：" + strings.Join(references, "、")}
	}
	state, operationID := api.TemplateStateDeleting, uuid.NewString()
	template.State, template.Error, template.OperationId = &state, nil, &operationID
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
	op, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: operationID, ScopeKind: "template", ScopeID: id, Kind: "delete-template", Payload: payload, ExpectedRevision: int32(template.Version)})
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
	w.Header().Set("Operation-Location", "/api/v1/operations/"+operationID)
	return writeJSON(w, http.StatusAccepted, result)
}
