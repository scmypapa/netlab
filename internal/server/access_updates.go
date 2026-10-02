package server

import (
	"context"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
)

func (s *Server) AccessUpdates(ctx context.Context) (func() error, error) {
	connection, err := pgx.ConnectConfig(ctx, s.Pool.Config().ConnConfig.Copy())
	if err != nil {
		return nil, err
	}
	if _, err = connection.Exec(ctx, "LISTEN netlab_access"); err != nil {
		connection.Close(ctx)
		return nil, err
	}
	return func() error {
		defer connection.Close(context.Background())
		for {
			notification, err := connection.WaitForNotification(ctx)
			if err != nil {
				return err
			}
			s.refreshSessions(ctx, notification.Payload)
		}
	}, nil
}

func (s *Server) refreshSessions(ctx context.Context, principalID string) {
	s.consoleMu.Lock()
	connections := map[*websocket.Conn]consoleOwner{}
	for connection, owner := range s.consoles {
		if owner.principal == principalID {
			connections[connection] = owner
		}
	}
	s.consoleMu.Unlock()
	for connection, owner := range connections {
		identity, err := s.Access.Authenticate(ctx, owner.credential)
		if err == nil {
			_, err = s.Environments.Authorized(ctx, identity, owner.environment, "session", owner.asset)
		}
		if err != nil {
			go connection.Close(websocket.StatusPolicyViolation, "访问授权已更新")
		}
	}
}

func (s *Server) Close() {
	s.consoleMu.Lock()
	defer s.consoleMu.Unlock()
	for connection := range s.consoles {
		connection.CloseNow()
	}
}
