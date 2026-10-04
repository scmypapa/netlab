package observation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/metrics"
	"netlab.local/core/internal/transport"
)

type Service struct {
	Pool    *pgxpool.Pool
	Client  *transport.Client
	Metrics *metrics.Store
}

// One controller owns the observation connections. PostgreSQL closes this
// session's advisory lock on disconnect; it does not occupy a pool slot.
func (s Service) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := s.lead(ctx); err != nil && ctx.Err() == nil {
			slog.Error("runtime observer stopped", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (s Service) lead(ctx context.Context) error {
	connection, err := pgx.ConnectConfig(ctx, s.Pool.Config().ConnConfig.Copy())
	if err != nil {
		return err
	}
	defer connection.Close(context.Background())
	for {
		var owner bool
		if err = connection.QueryRow(ctx, "SELECT pg_try_advisory_lock(73421493)").Scan(&owner); err != nil {
			return err
		}
		if owner {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if _, err = connection.Exec(ctx, "LISTEN netlab_nodes"); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	var waiting sync.WaitGroup
	defer func() { cancel(); waiting.Wait() }()
	waiting.Add(1)
	go func() { defer waiting.Done(); s.collectMetrics(ctx) }()
	type subscription struct {
		endpoint string
		cancel   context.CancelFunc
	}
	active := map[string]subscription{}
	for {
		nodes, err := queries.New(s.Pool).ListObservedNodes(ctx)
		if err != nil {
			return err
		}
		for _, node := range nodes {
			previous, exists := active[node.ID]
			if exists && previous.endpoint == node.Endpoint {
				continue
			}
			if exists {
				previous.cancel()
			}
			nodeCtx, stop := context.WithCancel(ctx)
			active[node.ID] = subscription{endpoint: node.Endpoint, cancel: stop}
			waiting.Add(1)
			go func() {
				defer waiting.Done()
				s.watch(nodeCtx, node)
			}()
		}
		wait, stop := context.WithTimeout(ctx, 15*time.Second)
		_, err = connection.WaitForNotification(wait)
		stop()
		if errors.Is(err, context.DeadlineExceeded) {
			// A keepalive on the lock connection cancels owned streams when the
			// database connection has died, before another controller takes over.
			keepalive, finish := context.WithTimeout(ctx, 3*time.Second)
			err = connection.Ping(keepalive)
			finish()
			if err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
	}
}

func (s Service) watch(ctx context.Context, node queries.ListObservedNodesRow) {
	q := queries.New(s.Pool)
	for ctx.Err() == nil {
		first := true
		err := s.Client.Observations(ctx, node.Endpoint, func(batch api.NodeObservation) error {
			if batch.NodeId != node.ID || (first && !batch.Snapshot) {
				return fmt.Errorf("node observation identity or initial snapshot does not match registered node")
			}
			first = false
			data, err := json.Marshal(batch.Results)
			if err != nil {
				return err
			}
			return q.ApplyNodeObservation(ctx, queries.ApplyNodeObservationParams{NodeID: node.ID, Results: data, Snapshot: batch.Snapshot, ObservedAt: pgtype.Timestamptz{Time: batch.ObservedAt, Valid: true}})
		})
		if ctx.Err() != nil {
			return
		}
		if saveErr := q.MarkObservedNodeOffline(ctx, node.ID); saveErr != nil {
			slog.Error("node observation disconnect", "node", node.ID, "error", saveErr)
		}
		slog.Warn("node observation disconnected", "node", node.ID, "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}
