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

type consoleOwner struct{ principal, environment string }

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
	s.consoleMu.Lock()
	if s.consoles == nil {
		s.consoles = map[*websocket.Conn]consoleOwner{}
	}
	s.consoles[backend] = consoleOwner{identity.Principal.ID, id}
	s.consoleMu.Unlock()
	defer func() { s.consoleMu.Lock(); delete(s.consoles, backend); s.consoleMu.Unlock() }()
	// Register before re-reading grants, so a concurrent revocation cannot miss this socket.
	identity, err = s.Access.Authenticate(r.Context(), credential(r))
	if err != nil {
		return err
	}
	if _, err = s.Environments.Authorized(r.Context(), identity, id, "session", asset); err != nil {
		return err
	}
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

func (s *Server) disconnectSessions(principalID, environmentID string) {
	s.consoleMu.Lock()
	var connections []*websocket.Conn
	for connection, owner := range s.consoles {
		if (principalID == "" || owner.principal == principalID) && (environmentID == "" || owner.environment == environmentID) {
			connections = append(connections, connection)
		}
	}
	s.consoleMu.Unlock()
	for _, connection := range connections {
		connection.Close(websocket.StatusPolicyViolation, "访问授权已更新")
	}
}
