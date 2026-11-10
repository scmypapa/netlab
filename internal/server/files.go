package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
)

func (s *Server) assetFiles(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	env, asset := r.PathValue("id"), r.PathValue("assetId")
	if _, err := s.Environments.Authorized(r.Context(), identity, env, "file", asset); err != nil {
		return err
	}
	current, err := s.Queries.GetCurrentAsset(r.Context(), queries.GetCurrentAssetParams{EnvironmentID: env, AssetID: asset})
	if err != nil {
		return err
	}
	var execution api.AssetExecution
	if err = json.Unmarshal(current.Execution, &execution); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	control := http.NewResponseController(w)
	if r.Method == http.MethodPut {
		if err = control.EnableFullDuplex(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
	}
	release, err := s.trackConnection(ctx, &accessConnection{principal: identity.Principal.ID, environment: env, asset: asset, credential: credential(r), permission: "file", close: func() { cancel(); control.SetReadDeadline(time.Now()); control.SetWriteDeadline(time.Now()) }})
	if err != nil {
		return err
	}
	defer release()
	endpoint := current.Endpoint
	headers := http.Header{}
	path := fmt.Sprintf("/node/v1/environments/%s/assets/%s/instances/%s/files", url.PathEscape(env), url.PathEscape(asset), url.PathEscape(current.InstanceID))
	if execution.Template.Kind == "vm" {
		endpoint, headers, err = s.sshTarget(ctx, identity, env, asset, nil)
		if err != nil {
			return err
		}
		path = "/node/v1/environments/" + url.PathEscape(env) + "/ssh/files"
	}
	if strings.HasSuffix(r.URL.Path, "/content") {
		path += "/content"
	}
	request, err := http.NewRequestWithContext(ctx, r.Method, endpoint+path+"?"+r.URL.Query().Encode(), r.Body)
	if err != nil {
		return err
	}
	request.Header = headers
	request.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	request.ContentLength = r.ContentLength
	return s.proxyFileResponse(w, r, request)
}

func (s *Server) proxyFileResponse(w http.ResponseWriter, r *http.Request, request *http.Request) error {
	response, err := s.Nodes.HTTP.Do(request)
	if err != nil {
		return httpError{http.StatusBadGateway, err.Error()}
	}
	defer response.Body.Close()
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Disposition"} {
		if value := response.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(response.StatusCode)
	count, err := io.Copy(w, response.Body)
	if err != nil || response.ContentLength >= 0 && count != response.ContentLength {
		if r.Context().Err() == nil {
			slog.WarnContext(r.Context(), "file proxy interrupted", "error", err, "bytes", count)
		}
		panic(http.ErrAbortHandler)
	}
	return nil
}
