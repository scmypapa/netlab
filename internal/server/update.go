package server

import (
	"errors"
	"net/http"

	"netlab.local/core/api"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/update"
)

func (s *Server) systemUpdate(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	status, err := s.Updates.Status()
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, status)
}

func (s *Server) checkSystemUpdate(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	if err := s.Updates.Check(r.Context()); err != nil {
		return httpError{http.StatusBadGateway, err.Error()}
	}
	return s.systemUpdate(w, r, identity)
}

func (s *Server) applySystemUpdate(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	var request api.ApplySystemUpdate
	if err := decode(w, r, &request); err != nil {
		return err
	}
	if err := s.Updates.Apply(r.Context(), request.Version); err != nil {
		if errors.Is(err, update.ErrConflict) || errors.Is(err, update.ErrNotInstalled) {
			return httpError{http.StatusConflict, err.Error()}
		}
		return httpError{http.StatusBadGateway, err.Error()}
	}
	status, err := s.Updates.Status()
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, status)
}
