package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"

	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/logfile"
)

func (s *Server) assetLogs(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	id, asset := r.PathValue("id"), r.PathValue("assetId")
	if _, err := s.Environments.Authorized(r.Context(), identity, id, "observe", asset); err != nil {
		return err
	}
	if _, err := logfile.Parse(r.URL.Query()); err != nil {
		return httpError{http.StatusBadRequest, err.Error()}
	}
	current, err := s.Queries.GetCurrentAsset(r.Context(), queries.GetCurrentAssetParams{EnvironmentID: id, AssetID: asset})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	release, err := s.trackConnection(ctx, &accessConnection{principal: identity.Principal.ID, environment: id, asset: asset, credential: credential(r), permission: "observe", close: cancel})
	if err != nil {
		return err
	}
	defer release()
	path := fmt.Sprintf("/node/v1/environments/%s/assets/%s/instances/%s/logs?%s", url.PathEscape(id), url.PathEscape(asset), current.InstanceID, r.URL.Query().Encode())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, current.Endpoint+path, nil)
	if err != nil {
		return err
	}
	response, err := s.Nodes.HTTP.Do(request)
	if err != nil {
		return httpError{http.StatusBadGateway, err.Error()}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		detail, err := io.ReadAll(io.LimitReader(response.Body, 16384))
		if err != nil {
			return err
		}
		return httpError{response.StatusCode, string(detail)}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	_, err = io.Copy(logStreamWriter{w, http.NewResponseController(w)}, response.Body)
	if err != nil && ctx.Err() == nil {
		slog.ErrorContext(ctx, "log stream interrupted", "environment", id, "asset", asset, "error", err)
	}
	return nil
}

type logStreamWriter struct {
	io.Writer
	control *http.ResponseController
}

func (w logStreamWriter) Write(data []byte) (int, error) {
	n, err := w.Writer.Write(data)
	if err == nil {
		err = w.control.Flush()
	}
	return n, err
}
