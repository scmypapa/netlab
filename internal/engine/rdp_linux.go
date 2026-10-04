//go:build linux

package engine

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"netlab.local/core/api"
	"netlab.local/core/internal/files"
	"netlab.local/core/internal/guest"
)

func (e *Engine) RDPAccess(w http.ResponseWriter, r *http.Request) {
	var target api.NodeRDP
	data, err := base64.StdEncoding.DecodeString(r.Header.Get("X-Netlab-Rdp"))
	if err == nil {
		err = json.Unmarshal(data, &target)
	}
	if err != nil {
		files.Failure(w, err, http.StatusBadRequest)
		return
	}
	address, err := netip.ParseAddr(target.Address)
	if err != nil || target.Settings.Port < 1 || target.Settings.Port > 65535 {
		files.Failure(w, net.InvalidAddrError("远程桌面地址或端口无效"), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if r.Method == http.MethodPost {
		probe, stop := context.WithTimeout(ctx, 15*time.Second)
		defer stop()
		connection, err := e.access.Dial(probe, r.PathValue("environmentId"), address, uint16(target.Settings.Port))
		if err != nil {
			files.Failure(w, err, http.StatusBadGateway)
			return
		}
		defer connection.Close()
		deadline, _ := probe.Deadline()
		connection.SetDeadline(deadline)
		fingerprint, err := guest.RDPCertificate(probe, connection)
		if err != nil {
			files.Failure(w, err, http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"fingerprint": fingerprint})
		return
	}
	if err = guest.ValidateRDP(target.Settings); err != nil {
		files.Failure(w, err, http.StatusBadRequest)
		return
	}
	width, height, dpi := 1280, 720, 96
	for key, destination := range map[string]*int{"width": &width, "height": &height, "dpi": &dpi} {
		if value := r.URL.Query().Get(key); value != "" {
			*destination, err = strconv.Atoi(value)
			if err != nil {
				files.Failure(w, err, http.StatusBadRequest)
				return
			}
		}
	}
	if width < 1 || height < 1 || width > 7680 || height > 4320 || dpi < 48 || dpi > 384 {
		files.Failure(w, net.InvalidAddrError("远程桌面尺寸无效"), http.StatusBadRequest)
		return
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		files.Failure(w, err, http.StatusBadGateway)
		return
	}
	defer listener.Close()
	go e.rdpForward(ctx, listener, r.PathValue("environmentId"), address, uint16(target.Settings.Port))
	connection, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "tcp", e.cfg.GuacdAddress)
	if err != nil {
		files.Failure(w, err, http.StatusBadGateway)
		return
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(15 * time.Second))
	stop := context.AfterFunc(ctx, func() { connection.Close(); listener.Close() })
	defer stop()
	reader := bufio.NewReaderSize(connection, 32<<10)
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	id, err := guest.OpenRDP(connection, reader, "127.0.0.1", port, target.Settings, width, height, dpi)
	if err != nil {
		files.Failure(w, err, http.StatusBadGateway)
		return
	}
	connection.SetDeadline(time.Time{})
	socket, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"guacamole"}})
	if err != nil {
		return
	}
	defer socket.CloseNow()
	socket.SetReadLimit(1 << 20)
	if err = socket.Write(ctx, websocket.MessageText, []byte(guest.GuacInstruction("", id))); err != nil {
		return
	}
	done := make(chan error, 2)
	go func() {
		for {
			frame, err := reader.ReadBytes(';')
			if len(frame) > 0 {
				if writeErr := socket.Write(ctx, websocket.MessageText, frame); writeErr != nil {
					done <- writeErr
					return
				}
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()
	go func() {
		for {
			kind, frame, err := socket.Read(ctx)
			if err == nil && kind != websocket.MessageText {
				err = net.InvalidAddrError("远程桌面使用文本协议")
			}
			if err == nil {
				if strings.HasPrefix(string(frame), "0.,4.ping,") {
					err = socket.Write(ctx, websocket.MessageText, frame)
				} else {
					_, err = connection.Write(frame)
				}
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()
	<-done
	socket.Close(websocket.StatusNormalClosure, "")
	cancel()
	connection.Close()
	<-done
}

func (e *Engine) rdpForward(ctx context.Context, listener net.Listener, env string, address netip.Addr, port uint16) {
	for {
		local, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer local.Close()
			remote, err := e.access.Dial(ctx, env, address, port)
			if err != nil {
				return
			}
			defer remote.Close()
			stop := context.AfterFunc(ctx, func() { local.Close(); remote.Close() })
			defer stop()
			done := make(chan struct{})
			go func() { io.Copy(remote, local); remote.Close(); local.Close(); close(done) }()
			io.Copy(local, remote)
			remote.Close()
			local.Close()
			<-done
		}()
	}
}
