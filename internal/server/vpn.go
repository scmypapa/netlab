package server

import (
	"net/http"
	"strconv"

	"netlab.local/core/api"
	"netlab.local/core/internal/access"
)

func (s *Server) listVPN(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	result, err := s.Environments.VPNList(r.Context(), identity, r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, result)
}
func (s *Server) createVPN(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	var body api.CreateVPNAccess
	if err := decode(w, r, &body); err != nil {
		return err
	}
	result, err := s.Environments.CreateVPN(r.Context(), identity, r.PathValue("id"), body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, result)
}
func (s *Server) revokeVPN(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	revision, err := strconv.Atoi(r.URL.Query().Get("expectedRevision"))
	if err != nil || revision < 0 {
		return httpError{http.StatusBadRequest, "请提供 expectedRevision"}
	}
	var requestID *string
	if value := r.URL.Query().Get("clientRequestId"); value != "" {
		requestID = &value
	}
	result, err := s.Environments.RevokeVPN(r.Context(), identity, r.PathValue("id"), r.PathValue("accessId"), revision, requestID)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, result)
}
func (s *Server) vpnConnection(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	result, err := s.Environments.VPNConnection(r.Context(), identity, r.PathValue("id"), r.PathValue("accessId"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, result)
}
