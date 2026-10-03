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

func (s *Server) captureTemplate(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	var request api.CaptureTemplate
	if err := decode(w, r, &request); err != nil {
		return err
	}
	if strings.TrimSpace(request.Name) == "" {
		return httpError{http.StatusBadRequest, "请输入模板名称"}
	}
	id, asset := r.PathValue("id"), r.PathValue("assetId")
	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(r.Context())
	q := s.Queries.WithTx(tx)
	row, err := q.LockEnvironment(r.Context(), id)
	if err != nil {
		return err
	}
	if int(row.Revision) != request.ExpectedRevision {
		return environment.ErrConflict
	}
	current, err := q.GetCurrentAsset(r.Context(), queries.GetCurrentAssetParams{EnvironmentID: id, AssetID: asset})
	if err != nil {
		return err
	}
	var execution api.AssetExecution
	if err = json.Unmarshal(current.Execution, &execution); err != nil {
		return err
	}
	if execution.Template.Kind != api.Vm || current.State != "stopped" {
		return httpError{http.StatusConflict, "请先关闭虚拟机，再固化模板"}
	}
	if row.Status == "deploying" || row.Status == "changing" || row.Status == "destroying" {
		return httpError{http.StatusConflict, "请等待当前任务完成"}
	}
	format, initialization, state := api.Qcow2, api.None, api.TemplateStateImporting
	template := api.Template{Id: uuid.NewString(), Name: strings.TrimSpace(request.Name), Kind: api.Vm, Os: execution.Template.Os, Version: 1,
		Source: "asset://" + id + "/" + asset + "/" + current.InstanceID, Hardware: execution.Template.Hardware, Resources: execution.Asset.Resources,
		Format: &format, Initialization: request.Initialization, State: &state, ArtifactNodeId: &current.NodeID}
	if template.Initialization == nil {
		template.Initialization = execution.Template.Initialization
		if template.Initialization == nil {
			template.Initialization = &initialization
		}
	}
	payload, err := json.Marshal(operation.Payload{Template: &template, BeforeStatus: row.Status, TemplateCapture: &api.TemplateCaptureSource{EnvironmentId: id, AssetId: asset, InstanceId: current.InstanceID}})
	if err != nil {
		return err
	}
	raw, err := json.Marshal(template)
	if err != nil {
		return err
	}
	if err = q.CreateTemplate(r.Context(), queries.CreateTemplateParams{ID: template.Id, Definition: raw}); err != nil {
		return err
	}
	op, err := q.CreateOperation(r.Context(), queries.CreateOperationParams{ID: uuid.NewString(), EnvironmentID: &id, ScopeKind: "environment", ScopeID: id, Kind: "capture-template", AssetID: &asset, Payload: payload, ExpectedRevision: row.Revision})
	if err != nil {
		return err
	}
	if err = q.SetEnvironmentOperation(r.Context(), queries.SetEnvironmentOperationParams{ID: id, OperationID: &op.ID, Status: "changing"}); err != nil {
		return err
	}
	if err = tx.Commit(r.Context()); err != nil {
		return err
	}
	w.Header().Set("Operation-Location", "/api/v1/operations/"+op.ID)
	return writeJSON(w, http.StatusCreated, template)
}
