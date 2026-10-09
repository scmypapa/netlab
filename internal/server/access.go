package server

import (
	"net/http"
	"strings"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
)

func (s *Server) listPrincipals(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if !identity.Administrator() {
		return access.ErrForbidden
	}
	cursor, limit, err := pagination(r)
	if err != nil {
		return err
	}
	rows, err := s.Queries.ListPrincipals(r.Context(), queries.ListPrincipalsParams{Cursor: cursor, PageLimit: limit, Search: strings.TrimSpace(r.URL.Query().Get("search")), Kind: r.URL.Query().Get("kind")})
	if err != nil {
		return err
	}
	result := make([]api.Principal, 0, len(rows))
	for _, p := range rows {
		item := api.Principal{Id: p.ID, Name: p.Name, Kind: api.PrincipalKind(p.Kind), Administrator: p.Administrator, Disabled: p.Disabled, CreatedAt: p.CreatedAt.Time}
		if p.ExpiresAt.Valid {
			item.ExpiresAt = &p.ExpiresAt.Time
		}
		result = append(result, item)
	}
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	var input api.CreateUser
	if err := decode(w, r, &input); err != nil {
		return err
	}
	result, err := s.Access.CreateUser(r.Context(), identity, input)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, result)
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	var input api.UpdateUser
	if err := decode(w, r, &input); err != nil {
		return err
	}
	id := r.PathValue("id")
	if err := s.Access.UpdateUser(r.Context(), identity, id, input); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	var input api.ChangePassword
	if err := decode(w, r, &input); err != nil {
		return err
	}
	if err := s.Access.ChangePassword(r.Context(), identity, input); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) principalGrants(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if !identity.Administrator() {
		return access.ErrForbidden
	}
	id := r.PathValue("id")
	if _, err := s.Queries.GetPrincipal(r.Context(), id); err != nil {
		return err
	}
	rows, err := s.Queries.GetGrants(r.Context(), id)
	if err != nil {
		return err
	}
	result := make([]api.ScopeGrant, 0, len(rows))
	for _, g := range rows {
		permissions := make([]api.Permission, len(g.Permissions))
		for i, p := range g.Permissions {
			permissions[i] = api.Permission(p)
		}
		result = append(result, api.ScopeGrant{ScopeKind: api.ScopeGrantScopeKind(g.ScopeKind), ScopeId: g.ScopeID, Permissions: permissions})
	}
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	var input api.CreateServiceToken
	if err := decode(w, r, &input); err != nil {
		return err
	}
	result, err := s.Access.CreateToken(r.Context(), identity, input)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, result)
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	id := r.PathValue("id")
	if err := s.Access.RevokeToken(r.Context(), identity, id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) getSharing(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	result, err := s.Access.Sharing(r.Context(), identity, r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) replaceSharing(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	var input []api.EnvironmentGrant
	if err := decode(w, r, &input); err != nil {
		return err
	}
	id := r.PathValue("id")
	if err := s.Access.ReplaceSharing(r.Context(), identity, id, input); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
