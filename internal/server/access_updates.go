package server

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"netlab.local/core/internal/access"
)

type accessConnection struct {
	principal, environment, asset, credential, permission string
	close                                                 func()
}

func (s *Server) trackConnection(ctx context.Context, owner *accessConnection) (func(), error) {
	s.connectionMu.Lock()
	if s.connections == nil {
		s.connections = map[*accessConnection]struct{}{}
	}
	s.connections[owner] = struct{}{}
	s.connectionMu.Unlock()
	remove := func() { s.connectionMu.Lock(); delete(s.connections, owner); s.connectionMu.Unlock() }
	// Register before re-reading grants, so concurrent revocation cannot miss the connection.
	identity, err := s.Access.Authenticate(ctx, owner.credential)
	if err == nil {
		err = s.authorizeConnection(ctx, identity, owner)
	}
	if err != nil {
		remove()
		return nil, err
	}
	var timer *time.Timer
	if identity.ExpiresAt != nil {
		timer = time.AfterFunc(time.Until(*identity.ExpiresAt), owner.close)
	}
	return func() {
		if timer != nil {
			timer.Stop()
		}
		remove()
	}, nil
}

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
	s.connectionMu.Lock()
	connections := []*accessConnection{}
	for owner := range s.connections {
		if owner.principal == principalID {
			connections = append(connections, owner)
		}
	}
	s.connectionMu.Unlock()
	for _, owner := range connections {
		identity, err := s.Access.Authenticate(ctx, owner.credential)
		if err == nil {
			err = s.authorizeConnection(ctx, identity, owner)
		}
		if err != nil {
			go owner.close()
		}
	}
}

func (s *Server) authorizeConnection(ctx context.Context, identity access.Identity, owner *accessConnection) error {
	if owner.permission == "read" && owner.asset == "" {
		_, _, err := s.Environments.Readable(ctx, identity, owner.environment)
		return err
	}
	_, err := s.Environments.Authorized(ctx, identity, owner.environment, owner.permission, owner.asset)
	return err
}

func (s *Server) Close() {
	s.connectionMu.Lock()
	connections := make([]*accessConnection, 0, len(s.connections))
	for connection := range s.connections {
		connections = append(connections, connection)
	}
	s.connectionMu.Unlock()
	for _, connection := range connections {
		connection.close()
	}
}
