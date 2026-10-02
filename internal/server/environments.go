package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/environment"
	"netlab.local/core/internal/operation"
)

func (s *Server) listEnvironments(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	cursor, limit, err := pagination(r)
	if err != nil {
		return err
	}
	principal := identity.Principal.ID
	rows, err := s.Queries.ListEnvironments(r.Context(), queries.ListEnvironmentsParams{IsAdmin: identity.Administrator(), IsUser: identity.Principal.Kind == "user", PrincipalID: &principal, Cursor: cursor, PageLimit: limit, Search: strings.TrimSpace(r.URL.Query().Get("search")), Status: r.URL.Query().Get("status")})
	if err != nil {
		return err
	}
	result := make([]api.EnvironmentSummary, 0, len(rows))
	for _, row := range rows {
		result = append(result, api.EnvironmentSummary{Id: row.ID, ProjectId: row.ProjectID, Name: row.Name, ExternalReference: row.ExternalReference, Revision: int(row.Revision), Status: api.EnvironmentStatus(row.Status), AssetCount: int(row.AssetCount), NetworkCount: int(row.NetworkCount), CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time})
	}
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) createEnvironment(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	var input api.CreateEnvironment
	if err := decode(w, r, &input); err != nil {
		return err
	}
	result, err := s.Environments.Create(r.Context(), identity, input)
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/api/v1/environments/"+result.Id)
	return writeJSON(w, http.StatusCreated, result)
}

func (s *Server) getEnvironment(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	row, visible, err := s.Environments.Readable(r.Context(), identity, r.PathValue("id"))
	if err != nil {
		return err
	}
	result, err := environment.VisibleRecord(row, visible)
	if err != nil {
		return err
	}
	permissions := identity.Permissions(row.ProjectID, row.ID, "", row.OwnerID)
	result.Permissions = &permissions
	assetPermissions := map[string][]api.Permission{}
	spec := result.Spec
	if result.AppliedSpec != nil {
		spec = *result.AppliedSpec
	}
	for _, asset := range spec.Assets {
		assetPermissions[asset.Id] = identity.Permissions(row.ProjectID, row.ID, asset.Id, row.OwnerID)
	}
	result.AssetPermissions = &assetPermissions
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) environmentState(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	row, visible, err := s.Environments.Readable(r.Context(), identity, r.PathValue("id"))
	if err != nil {
		return err
	}
	assets, err := s.Queries.ListRuntimeAssetStates(r.Context(), row.ID)
	if err != nil {
		return err
	}
	current := make(map[string]queries.ListRuntimeAssetStatesRow, len(assets))
	for _, asset := range assets {
		if visible != nil && !visible[asset.AssetID] {
			continue
		}
		previous, exists := current[asset.AssetID]
		if asset.Current || !exists || (!previous.Current && asset.ObservedAt.Time.After(previous.ObservedAt.Time)) {
			current[asset.AssetID] = asset
		}
	}
	result := api.EnvironmentState{Id: row.ID, Status: row.Status, Revision: int(row.Revision), UpdatedAt: row.UpdatedAt.Time, Assets: make([]api.AssetState, 0, len(current))}
	for _, asset := range current {
		var message *string
		if asset.Error != "" {
			message = &asset.Error
		}
		result.Assets = append(result.Assets, api.AssetState{AssetId: asset.AssetID, InstanceId: asset.InstanceID, NodeId: asset.NodeID, State: asset.State, Error: message, ObservedAt: asset.ObservedAt.Time})
	}
	slices.SortFunc(result.Assets, func(a, b api.AssetState) int { return strings.Compare(a.AssetId, b.AssetId) })
	if row.OperationID != nil {
		op, err := s.Queries.GetOperation(r.Context(), *row.OperationID)
		if err != nil {
			return err
		}
		if visible == nil || (op.AssetID != nil && visible[*op.AssetID]) {
			record, err := environment.Operation(op)
			if err != nil {
				return err
			}
			record.Retryable = operation.Retryable(identity, op, row)
			if visible != nil && record.Results != nil {
				filtered := slices.DeleteFunc(*record.Results, func(item api.ExecutionResult) bool { return !visible[item.AssetId] })
				record.Results = &filtered
			}
			result.Operation = &record
		}
	}
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) environmentAction(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	var input api.ActionRequest
	if err := decode(w, r, &input); err != nil {
		return err
	}
	result, err := s.Environments.Action(r.Context(), identity, r.PathValue("id"), r.PathValue("assetId"), input)
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/api/v1/operations/"+result.Id)
	return writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) environmentChanges(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	var input api.ChangeRequest
	if err := decode(w, r, &input); err != nil {
		return err
	}
	preview, operation, err := s.Environments.Change(r.Context(), identity, r.PathValue("id"), input)
	if err != nil {
		return err
	}
	if operation == nil {
		return writeJSON(w, http.StatusOK, preview)
	}
	w.Header().Set("Location", "/api/v1/operations/"+operation.Id)
	return writeJSON(w, http.StatusAccepted, operation)
}

func (s *Server) saveView(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	id := r.PathValue("id")
	if _, err := s.Environments.Authorized(r.Context(), identity, id, "compose", ""); err != nil {
		return err
	}
	var input api.CanvasView
	if err := decode(w, r, &input); err != nil {
		return err
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	if err = s.Queries.SaveView(r.Context(), queries.SaveViewParams{ID: id, View: raw}); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) saveDraft(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	id := r.PathValue("id")
	_, err := s.Environments.Authorized(r.Context(), identity, id, "compose", "")
	if err != nil {
		return err
	}
	var input api.Draft
	if err := decode(w, r, &input); err != nil {
		return err
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	if err = s.Queries.SaveDraft(r.Context(), queries.SaveDraftParams{ID: id, Draft: raw}); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) discardDraft(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	id := r.PathValue("id")
	if _, err := s.Environments.Authorized(r.Context(), identity, id, "compose", ""); err != nil {
		return err
	}
	if err := s.Queries.SaveDraft(r.Context(), queries.SaveDraftParams{ID: id}); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) listOperations(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	id := r.URL.Query().Get("environmentId")
	cursor, limit, err := pagination(r)
	if err != nil {
		return err
	}
	principal := identity.Principal.ID
	rows, err := s.Queries.ListVisibleOperations(r.Context(), queries.ListVisibleOperationsParams{EnvironmentID: id, IsAdmin: identity.Administrator(), IsUser: identity.Principal.Kind == "user", PrincipalID: &principal, Cursor: cursor, PageLimit: limit})
	if err != nil {
		return err
	}
	result := make([]api.Operation, 0, len(rows))
	for _, row := range rows {
		item, err := environment.Operation(row.Operation)
		if err != nil {
			return err
		}
		item.Retryable = operation.Retryable(identity, row.Operation, queries.Environment{ProjectID: row.ProjectID, OwnerID: row.OwnerID, OperationID: row.CurrentOperationID})
		result = append(result, item)
	}
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) getOperation(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	row, err := s.Queries.GetOperation(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	var runtime queries.Environment
	if row.EnvironmentID != nil {
		if runtime, err = s.Queries.GetEnvironment(r.Context(), *row.EnvironmentID); err != nil {
			return err
		}
		if !operation.Readable(identity, row, runtime) {
			return access.ErrForbidden
		}
	} else if err = requireAdministrator(identity); err != nil {
		return err
	}
	result, err := environment.Operation(row)
	if err != nil {
		return err
	}
	result.Retryable = operation.Retryable(identity, row, runtime)
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) retryOperation(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	result, err := (operation.Service{Pool: s.Pool, Queries: s.Queries}).Retry(r.Context(), identity, r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, result)
}
