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

func (s *Server) assetSSHSettings(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	env, asset := r.PathValue("id"), r.PathValue("assetId")
	row, err := s.Queries.GetEnvironment(r.Context(), env)
	if err != nil {
		return err
	}
	if !identity.Allows("session", row.ProjectID, env, asset, row.OwnerID) && !identity.Allows("file", row.ProjectID, env, asset, row.OwnerID) {
		return access.ErrForbidden
	}
	if _, err = s.Queries.GetCurrentAsset(r.Context(), queries.GetCurrentAssetParams{EnvironmentID: env, AssetID: asset}); err != nil {
		return err
	}
	key := identity.Principal.ID + "/" + env + "/" + asset
	lookup := queries.GetGuestConnectionParams{PrincipalID: identity.Principal.ID, EnvironmentID: env, AssetID: asset}
	if r.Method == http.MethodDelete {
		if err = s.Queries.DeleteGuestConnection(r.Context(), queries.DeleteGuestConnectionParams(lookup)); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	saved, err := s.readGuestSettings(r.Context(), lookup, key)
	if r.Method == http.MethodGet {
		if err != nil {
			return err
		}
		saved.Password, saved.PrivateKey, saved.Passphrase = nil, nil, nil
		return writeJSON(w, http.StatusOK, saved)
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var settings api.SSHSettings
	if err = decode(w, r, &settings); err != nil {
		return err
	}
	if saved.AuthKind == settings.AuthKind {
		if settings.AuthKind == "password" && settings.Password == nil {
			settings.Password = saved.Password
		}
		if settings.AuthKind == "key" && settings.PrivateKey == nil {
			settings.PrivateKey = saved.PrivateKey
			if settings.Passphrase == nil {
				settings.Passphrase = saved.Passphrase
			}
		}
	}
	if err = guest.Validate(settings); err != nil {
		return httpError{http.StatusBadRequest, err.Error()}
	}
	if settings.AuthKind == "password" {
		settings.PrivateKey, settings.Passphrase = nil, nil
	} else {
		settings.Password = nil
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	if err = s.Queries.PutGuestConnection(r.Context(), queries.PutGuestConnectionParams{PrincipalID: identity.Principal.ID, EnvironmentID: env, AssetID: asset, Encrypted: s.Secrets.Encrypt(raw, key)}); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) readGuestSettings(ctx context.Context, lookup queries.GetGuestConnectionParams, key string) (api.SSHSettings, error) {
	var settings api.SSHSettings
	raw, err := s.Queries.GetGuestConnection(ctx, lookup)
	if err != nil {
		return settings, err
	}
	raw, err = s.Secrets.Decrypt(raw, key)
	if err == nil {
		err = json.Unmarshal(raw, &settings)
	}
	return settings, err
}

func (s *Server) sshTarget(ctx context.Context, identity access.Identity, env, asset string, probe *api.SSHProbe) (string, http.Header, error) {
	var settings api.SSHSettings
	var err error
	if probe != nil {
		settings.Port, settings.InterfaceId = probe.Port, probe.InterfaceId
	} else {
		settings, err = s.readGuestSettings(ctx, queries.GetGuestConnectionParams{PrincipalID: identity.Principal.ID, EnvironmentID: env, AssetID: asset}, identity.Principal.ID+"/"+env+"/"+asset)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil, httpError{http.StatusConflict, "请设置 SSH 连接"}
		}
		if err != nil {
			return "", nil, err
		}
	}
	current, err := s.Queries.GetCurrentAsset(ctx, queries.GetCurrentAssetParams{EnvironmentID: env, AssetID: asset})
	if err != nil {
		return "", nil, err
	}
	var execution api.AssetExecution
	if err = json.Unmarshal(current.Execution, &execution); err != nil {
		return "", nil, err
	}
	var address string
	for _, item := range execution.Asset.Interfaces {
		if settings.InterfaceId != nil && item.Id == *settings.InterfaceId || settings.InterfaceId == nil && item.Primary {
			address = item.Address
			break
		}
	}
	if address == "" {
		return "", nil, httpError{http.StatusConflict, "资产没有可用于 SSH 的网络地址"}
	}
	endpoint, err := s.Queries.GetEnvironmentNetworkEndpoint(ctx, env)
	if err != nil {
		return "", nil, err
	}
	raw, err := json.Marshal(api.NodeSSH{Address: address, Settings: settings})
	if err != nil {
		return "", nil, err
	}
	return endpoint, http.Header{"X-Netlab-Ssh": []string{base64.StdEncoding.EncodeToString(raw)}}, nil
}

func (s *Server) assetSSHHostKey(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	env, asset := r.PathValue("id"), r.PathValue("assetId")
	row, err := s.Queries.GetEnvironment(r.Context(), env)
	if err != nil {
		return err
	}
	if !identity.Allows("session", row.ProjectID, env, asset, row.OwnerID) && !identity.Allows("file", row.ProjectID, env, asset, row.OwnerID) {
		return access.ErrForbidden
	}
	var probe api.SSHProbe
	if err = decode(w, r, &probe); err != nil {
		return err
	}
	if probe.Port < 1 || probe.Port > 65535 {
		return httpError{http.StatusBadRequest, "请填写有效端口"}
	}
	endpoint, headers, err := s.sshTarget(r.Context(), identity, env, asset, &probe)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint+"/node/v1/environments/"+url.PathEscape(env)+"/ssh/host-key", nil)
	if err != nil {
		return err
	}
	request.Header = headers
	return s.proxyFileResponse(w, r, request)
}
