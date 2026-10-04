package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/jackc/pgx/v5"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/guest"
)

func (s *Server) assetRDPSettings(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	env, asset := r.PathValue("id"), r.PathValue("assetId")
	if _, err := s.Environments.Authorized(r.Context(), identity, env, "session", asset); err != nil {
		return err
	}
	if _, err := s.Queries.GetCurrentAsset(r.Context(), queries.GetCurrentAssetParams{EnvironmentID: env, AssetID: asset}); err != nil {
		return err
	}
	lookup := queries.GetGuestConnectionParams{PrincipalID: identity.Principal.ID, EnvironmentID: env, AssetID: asset, Protocol: "rdp"}
	if r.Method == http.MethodDelete {
		if err := s.Queries.DeleteGuestConnection(r.Context(), queries.DeleteGuestConnectionParams(lookup)); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	saved, err := s.readRDPSettings(r.Context(), lookup)
	if r.Method == http.MethodGet {
		if err != nil {
			return err
		}
		saved.Password = nil
		return writeJSON(w, http.StatusOK, saved)
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var settings api.RDPSettings
	if err = decode(w, r, &settings); err != nil {
		return err
	}
	if settings.Password == nil {
		settings.Password = saved.Password
	}
	if err = guest.ValidateRDP(settings); err != nil {
		return httpError{http.StatusBadRequest, err.Error()}
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	key := identity.Principal.ID + "/" + env + "/" + asset + "/rdp"
	if err = s.Queries.PutGuestConnection(r.Context(), queries.PutGuestConnectionParams{PrincipalID: lookup.PrincipalID, EnvironmentID: env, AssetID: asset, Protocol: "rdp", Encrypted: s.Secrets.Encrypt(raw, key)}); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) readRDPSettings(ctx context.Context, lookup queries.GetGuestConnectionParams) (api.RDPSettings, error) {
	var settings api.RDPSettings
	raw, err := s.Queries.GetGuestConnection(ctx, lookup)
	if err != nil {
		return settings, err
	}
	raw, err = s.Secrets.Decrypt(raw, lookup.PrincipalID+"/"+lookup.EnvironmentID+"/"+lookup.AssetID+"/rdp")
	if err == nil {
		err = json.Unmarshal(raw, &settings)
	}
	return settings, err
}

func (s *Server) rdpTarget(ctx context.Context, identity access.Identity, env, asset string, probe *api.SSHProbe) (string, http.Header, error) {
	var settings api.RDPSettings
	var err error
	if probe != nil {
		settings.Port, settings.InterfaceId = probe.Port, probe.InterfaceId
	} else {
		settings, err = s.readRDPSettings(ctx, queries.GetGuestConnectionParams{PrincipalID: identity.Principal.ID, EnvironmentID: env, AssetID: asset, Protocol: "rdp"})
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil, httpError{http.StatusConflict, "请设置远程桌面连接"}
		}
		if err != nil {
			return "", nil, err
		}
	}
	endpoint, address, err := s.guestAddress(ctx, env, asset, settings.InterfaceId)
	if err != nil {
		return "", nil, err
	}
	raw, err := json.Marshal(api.NodeRDP{Address: address, Settings: settings})
	return endpoint, http.Header{"X-Netlab-Rdp": []string{base64.StdEncoding.EncodeToString(raw)}}, err
}

func (s *Server) assetRDPCertificate(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	env, asset := r.PathValue("id"), r.PathValue("assetId")
	if _, err := s.Environments.Authorized(r.Context(), identity, env, "session", asset); err != nil {
		return err
	}
	var probe api.SSHProbe
	if err := decode(w, r, &probe); err != nil {
		return err
	}
	if probe.Port < 1 || probe.Port > 65535 {
		return httpError{http.StatusBadRequest, "请填写有效端口"}
	}
	endpoint, headers, err := s.rdpTarget(r.Context(), identity, env, asset, &probe)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint+"/node/v1/environments/"+url.PathEscape(env)+"/rdp/certificate", nil)
	if err != nil {
		return err
	}
	request.Header = headers
	return s.proxyFileResponse(w, r, request)
}
