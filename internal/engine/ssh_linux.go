//go:build linux

package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"netlab.local/core/api"
	"netlab.local/core/internal/files"
	"netlab.local/core/internal/guest"
	"netlab.local/core/internal/stream"
)

func (e *Engine) GuestAccess(w http.ResponseWriter, r *http.Request) {
	var target api.NodeSSH
	data, err := base64.StdEncoding.DecodeString(r.Header.Get("X-Netlab-SSH"))
	if err == nil {
		err = json.Unmarshal(data, &target)
	}
	if err != nil {
		files.Failure(w, err, http.StatusBadRequest)
		return
	}
	address, err := netip.ParseAddr(target.Address)
	if err != nil || target.Settings.Port < 1 || target.Settings.Port > 65535 {
		files.Failure(w, net.InvalidAddrError("SSH 地址或端口无效"), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	raw, err := e.access.Dial(ctx, r.PathValue("environmentId"), address, uint16(target.Settings.Port))
	cancel()
	if err != nil {
		files.Failure(w, err, http.StatusBadGateway)
		return
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(15 * time.Second))
	// Keep the SSH channel alive briefly so an interrupted upload can remove its staging file.
	stop := context.AfterFunc(r.Context(), func() {
		raw.SetDeadline(time.Now().Add(3 * time.Second))
		http.NewResponseController(w).SetReadDeadline(time.Now())
	})
	defer stop()
	endpoint := net.JoinHostPort(target.Address, strconv.Itoa(target.Settings.Port))
	if r.PathValue("kind") == "host-key" {
		fingerprint, err := guest.HostKey(raw, endpoint)
		if err != nil {
			files.Failure(w, err, http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"fingerprint": fingerprint})
		return
	}
	connection, err := guest.Connect(raw, endpoint, target.Settings)
	if err != nil {
		files.Failure(w, err, http.StatusBadGateway)
		return
	}
	defer connection.Close()
	raw.SetDeadline(time.Time{})
	if err = r.Context().Err(); err != nil {
		files.Failure(w, err, http.StatusBadGateway)
		return
	}
	if r.PathValue("kind") == "console" {
		session, err := connection.NewSession()
		if err != nil {
			files.Failure(w, err, http.StatusBadGateway)
			return
		}
		defer session.Close()
		output, writer := io.Pipe()
		defer output.Close()
		defer writer.Close()
		input, err := session.StdinPipe()
		if err == nil {
			err = session.RequestPty("xterm-256color", 24, 80, nil)
		}
		session.Stdout, session.Stderr = writer, writer
		if err == nil {
			err = session.Shell()
		}
		if err != nil {
			files.Failure(w, err, http.StatusBadGateway)
			return
		}
		go func() { err := session.Wait(); writer.CloseWithError(err) }()
		console := &sshConsole{Reader: output, Writer: input, close: session.Close}
		socket, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"binary"}})
		if err != nil {
			return
		}
		defer socket.CloseNow()
		stream.Console(r.Context(), socket, console, func(_ context.Context, cols, rows uint32) error { return session.WindowChange(int(rows), int(cols)) })
		return
	}
	store, err := files.OpenSFTP(connection)
	if err != nil {
		files.Failure(w, err, http.StatusBadGateway)
		return
	}
	defer store.Close()
	files.Serve(w, r, store)
}

type sshConsole struct {
	io.Reader
	io.Writer
	close func() error
}

func (c *sshConsole) Close() error { return c.close() }
