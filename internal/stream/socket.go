package stream

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/coder/websocket"
	"netlab.local/core/api"
)

func Relay(ctx context.Context, a, b *websocket.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 2)
	copyMessages := func(dst, src *websocket.Conn) {
		for {
			kind, data, err := src.Read(ctx)
			if err == nil {
				err = dst.Write(ctx, kind, data)
			}
			if err != nil {
				done <- err
				return
			}
		}
	}
	go copyMessages(a, b)
	go copyMessages(b, a)
	err := <-done
	status := websocket.CloseStatus(err)
	if status < 0 {
		status = websocket.StatusInternalError
	}
	a.Close(status, "")
	b.Close(status, "")
	cancel()
	<-done
}

func Console(ctx context.Context, socket *websocket.Conn, connection io.ReadWriteCloser, resize func(context.Context, uint32, uint32) error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 2)
	go func() {
		buffer := make([]byte, 32<<10)
		for {
			n, err := connection.Read(buffer)
			if n > 0 {
				if writeErr := socket.Write(ctx, websocket.MessageBinary, buffer[:n]); writeErr != nil {
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
			kind, data, err := socket.Read(ctx)
			if err == nil && kind == websocket.MessageBinary {
				_, err = connection.Write(data)
			} else if err == nil && resize != nil {
				var size api.ConsoleResize
				err = json.Unmarshal(data, &size)
				if err == nil && (size.Cols < 1 || size.Rows < 1 || size.Cols > 1000 || size.Rows > 1000) {
					err = errors.New("invalid terminal dimensions")
				}
				if err == nil {
					err = resize(ctx, uint32(size.Cols), uint32(size.Rows))
				}
			} else if err == nil {
				err = errors.New("binary console messages required")
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()
	err := <-done
	status := websocket.CloseStatus(err)
	if errors.Is(err, io.EOF) {
		status = websocket.StatusNormalClosure
	} else if status < 0 {
		status = websocket.StatusInternalError
	}
	socket.Close(status, "")
	cancel()
	connection.Close()
	<-done
}
