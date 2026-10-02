package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/stream"
)

func (s *Server) assetConsole(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	id, asset := r.PathValue("id"), r.PathValue("assetId")
	if _, err := s.Environments.Authorized(r.Context(), identity, id, "session", asset); err != nil {
		return err
	}
	kind := r.URL.Query().Get("kind")
	if kind != "terminal" && kind != "serial" && kind != "vnc" {
		return httpError{http.StatusBadRequest, "请选择终端、串口或 VNC"}
	}
	current, err := s.Queries.GetCurrentAsset(r.Context(), queries.GetCurrentAssetParams{EnvironmentID: id, AssetID: asset})
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/node/v1/environments/%s/assets/%s/instances/%s/console?kind=%s", url.PathEscape(id), url.PathEscape(asset), current.InstanceID, kind)
	endpoint := "wss" + strings.TrimPrefix(current.Endpoint, "https") + path
	backend, response, err := websocket.Dial(r.Context(), endpoint, &websocket.DialOptions{HTTPClient: s.Nodes.HTTP, Subprotocols: []string{"binary"}})
	if err != nil {
		if response != nil && response.StatusCode == http.StatusConflict {
			return httpError{http.StatusConflict, "资产当前无法打开控制台"}
		}
		return httpError{http.StatusBadGateway, err.Error()}
	}
	defer backend.CloseNow()
	release, err := s.trackConnection(r.Context(), &accessConnection{
		principal: identity.Principal.ID, environment: id, asset: asset, credential: credential(r), permission: "session",
		close: func() { backend.Close(websocket.StatusPolicyViolation, "访问授权已更新") },
	})
	if err != nil {
		return err
	}
	defer release()
	frontend, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"binary"}})
	if err != nil {
		return nil
	}
	defer frontend.CloseNow()
	frontend.SetReadLimit(1 << 20)
	backend.SetReadLimit(1 << 20)
	stream.Relay(r.Context(), frontend, backend)
	return nil
}
