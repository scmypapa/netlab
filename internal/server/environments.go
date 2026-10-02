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
	rows, err := s.Queries.ListEnvironments(r.Context(), queries.ListEnvironmentsParams{IsAdmin: identity.Administrator(), PrincipalID: &principal, Cursor: cursor, PageLimit: limit})
	if err != nil {
		return err
	}
	result := make([]api.Environment, 0, len(rows))
	for _, row := range rows {
		item, err := environment.Record(row)
		if err != nil {
			return err
		}
		result = append(result, item)
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
	row, err := s.Environments.Authorized(r.Context(), identity, r.PathValue("id"), "read", "")
	if err != nil {
		return err
	}
	result, err := environment.Record(row)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) environmentState(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	row, err := s.Environments.Authorized(r.Context(), identity, r.PathValue("id"), "read", "")
	if err != nil {
		return err
	}
	assets, err := s.Queries.ListRuntimeAssets(r.Context(), row.ID)
	if err != nil {
		return err
	}
	current := make(map[string]queries.RuntimeAsset, len(assets))
	for _, asset := range assets {
		previous, exists := current[asset.AssetID]
		if asset.Current || !exists || (!previous.Current && asset.ObservedAt.Time.After(previous.ObservedAt.Time)) {
			current[asset.AssetID] = asset
		}
	}
	result := api.EnvironmentState{Id: row.ID, Status: row.Status, Revision: int(row.Revision), UpdatedAt: row.UpdatedAt.Time, Assets: make([]api.AssetState, 0, len(current))}
	for _, asset := range current {
		result.Assets = append(result.Assets, api.AssetState{AssetId: asset.AssetID, InstanceId: asset.InstanceID, NodeId: asset.NodeID, State: asset.State, Error: asset.Error, ObservedAt: asset.ObservedAt.Time})
	}
	slices.SortFunc(result.Assets, func(a, b api.AssetState) int { return strings.Compare(a.AssetId, b.AssetId) })
	if row.OperationID != nil {
		op, err := s.Queries.GetOperation(r.Context(), *row.OperationID)
		if err != nil {
			return err
		}
		record, err := environment.Operation(op)
		if err != nil {
			return err
		}
		result.Operation = &record
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
	rows, err := s.Queries.ListVisibleOperations(r.Context(), queries.ListVisibleOperationsParams{EnvironmentID: id, IsAdmin: identity.Administrator(), PrincipalID: &principal, Cursor: cursor, PageLimit: limit})
	if err != nil {
		return err
	}
	result := make([]api.Operation, 0, len(rows))
	for _, row := range rows {
		item, err := environment.Operation(row)
		if err != nil {
			return err
		}
		result = append(result, item)
	}
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) getOperation(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	row, err := s.Queries.GetOperation(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	if row.EnvironmentID != nil {
		asset := ""
		if row.AssetID != nil {
			asset = *row.AssetID
		}
		if _, err = s.Environments.Authorized(r.Context(), identity, *row.EnvironmentID, "read", asset); err != nil {
			return err
		}
	} else if err = requireAdministrator(identity); err != nil {
		return err
	}
	result, err := environment.Operation(row)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) retryOperation(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	result, err := (operation.Service{Pool: s.Pool, Queries: s.Queries}).Retry(r.Context(), identity, r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, result)
}
