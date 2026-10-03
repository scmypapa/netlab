package server

import (
	"net/http"
	"strings"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/blueprint"
)

func (s *Server) listBlueprints(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	cursor, limit, err := pagination(r)
	if err != nil {
		return err
	}
	principal := identity.Principal.ID
	rows, err := s.Queries.ListBlueprints(r.Context(), queries.ListBlueprintsParams{IsAdmin: identity.Administrator(), IsUser: identity.Principal.Kind == "user", PrincipalID: &principal, Cursor: cursor, PageLimit: limit, Search: strings.TrimSpace(r.URL.Query().Get("search"))})
	if err != nil {
		return err
	}
	result := make([]api.Blueprint, 0, len(rows))
	for _, row := range rows {
		result = append(result, blueprint.Record(queries.GetBlueprintRow(row), identity))
	}
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) getBlueprint(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	row, err := (blueprint.Service{Pool: s.Pool, Queries: s.Queries}).Authorized(r.Context(), identity, r.PathValue("id"), "read")
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, blueprint.Record(row, identity))
}

func (s *Server) deleteBlueprint(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := (blueprint.Service{Pool: s.Pool, Queries: s.Queries}).Delete(r.Context(), identity, r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) listBlueprintVersions(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	id := r.PathValue("id")
	if _, err := (blueprint.Service{Pool: s.Pool, Queries: s.Queries}).Authorized(r.Context(), identity, id, "read"); err != nil {
		return err
	}
	cursor, limit, err := pagination(r)
	if err != nil {
		return err
	}
	rows, err := s.Queries.ListBlueprintVersions(r.Context(), queries.ListBlueprintVersionsParams{BlueprintID: id, Cursor: cursor, PageLimit: limit})
	if err != nil {
		return err
	}
	result := make([]api.BlueprintVersionSummary, 0, len(rows))
	for _, row := range rows {
		result = append(result, api.BlueprintVersionSummary{Id: row.ID, BlueprintId: row.BlueprintID, Version: int(row.Version), AssetCount: int(row.AssetCount), NetworkCount: int(row.NetworkCount), CreatedAt: row.CreatedAt.Time})
	}
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) getBlueprintVersion(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	result, err := (blueprint.Service{Pool: s.Pool, Queries: s.Queries}).GetVersion(r.Context(), identity, r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) saveEnvironmentBlueprint(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	var input api.SaveBlueprint
	if err := decode(w, r, &input); err != nil {
		return err
	}
	result, _, err := (blueprint.Service{Pool: s.Pool, Queries: s.Queries}).Save(r.Context(), identity, r.PathValue("id"), "", input.Name, input.ExpectedRevision, input.Spec)
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/api/v1/blueprints/"+result.Id)
	return writeJSON(w, http.StatusCreated, result)
}

func (s *Server) saveBlueprintVersion(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	var input api.SaveBlueprintVersion
	if err := decode(w, r, &input); err != nil {
		return err
	}
	_, result, err := (blueprint.Service{Pool: s.Pool, Queries: s.Queries}).Save(r.Context(), identity, input.EnvironmentId, r.PathValue("id"), "", input.ExpectedRevision, input.Spec)
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/api/v1/blueprint-versions/"+result.Id)
	return writeJSON(w, http.StatusCreated, result)
}
