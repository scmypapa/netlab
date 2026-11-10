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
	if kind != "terminal" && kind != "serial" && kind != "vnc" && kind != "ssh" && kind != "rdp" {
		return httpError{http.StatusBadRequest, "请选择终端、串口、VNC、SSH 或远程桌面"}
	}
	var endpoint string
	var headers http.Header
	var err error
	if kind == "ssh" {
		endpoint, headers, err = s.sshTarget(r.Context(), identity, id, asset, nil)
		endpoint += "/node/v1/environments/" + url.PathEscape(id) + "/ssh/console"
	} else if kind == "rdp" {
		endpoint, headers, err = s.rdpTarget(r.Context(), identity, id, asset, nil)
		query := url.Values{}
		for _, name := range []string{"width", "height", "dpi"} {
			if value := r.URL.Query().Get(name); value != "" {
				query.Set(name, value)
			}
		}
		endpoint += "/node/v1/environments/" + url.PathEscape(id) + "/rdp/console?" + query.Encode()
	} else {
		var current queries.GetCurrentAssetRow
		current, err = s.Queries.GetCurrentAsset(r.Context(), queries.GetCurrentAssetParams{EnvironmentID: id, AssetID: asset})
		endpoint = current.Endpoint + fmt.Sprintf("/node/v1/environments/%s/assets/%s/instances/%s/console?kind=%s", url.PathEscape(id), url.PathEscape(asset), current.InstanceID, kind)
	}
	if err != nil {
		return err
	}
	endpoint = "wss" + strings.TrimPrefix(endpoint, "https")
	protocol := "binary"
	if kind == "rdp" {
		protocol = "guacamole"
	}
	backend, response, err := websocket.Dial(r.Context(), endpoint, &websocket.DialOptions{HTTPClient: s.Nodes.HTTP, HTTPHeader: headers, Subprotocols: []string{protocol}})
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
	frontend, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{protocol}})
	if err != nil {
		return nil
	}
	defer frontend.CloseNow()
	frontend.SetReadLimit(1 << 20)
	backend.SetReadLimit(1 << 20)
	stream.Relay(r.Context(), frontend, backend)
	return nil
}
