package observation

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"netlab.local/core/db/queries"
)

// Metric storage is independent from runtime observations and never marks a node offline.
func (s Service) collectMetrics(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		nodes, err := queries.New(s.Pool).ListNodes(ctx)
		if err != nil {
			slog.Error("metrics node query failed", "error", err)
		}
		var waiting sync.WaitGroup
		for _, node := range nodes {
			if node.State != "ready" {
				continue
			}
			waiting.Add(1)
			go func() {
				defer waiting.Done()
				if err := s.nodeMetrics(ctx, node.Endpoint); err != nil && ctx.Err() == nil {
					slog.Error("node metrics collection failed", "node", node.ID, "error", err)
				}
			}()
		}
		waiting.Wait()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s Service) nodeMetrics(ctx context.Context, endpoint string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/node/v1/metrics", nil)
	if err != nil {
		return err
	}
	response, err := s.Client.HTTP.Do(r)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		detail, err := io.ReadAll(io.LimitReader(response.Body, 16384))
		if err != nil {
			return err
		}
		return fmt.Errorf("node metrics returned %d: %s", response.StatusCode, detail)
	}
	return s.Metrics.Import(ctx, response.Body)
}
