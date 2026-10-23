package observation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
)

func (s Service) collectSecurity(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	pruned := time.Time{}
	failures := map[string]string{}
	for ctx.Err() == nil {
		if err := s.securityCycle(ctx, failures); err != nil && ctx.Err() == nil {
			slog.Error("security collection", "error", err)
		}
		if time.Since(pruned) >= time.Hour {
			if err := queries.New(s.Pool).PruneSecurityAlerts(ctx); err != nil {
				slog.Error("security retention", "error", err)
			} else {
				pruned = time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s Service) securityCycle(ctx context.Context, failures map[string]string) error {
	q := queries.New(s.Pool)
	targets, err := q.ListSecurityTargets(ctx)
	if err != nil {
		return err
	}
	interfaces := make([]api.SecurityInterface, 0)
	for _, target := range targets {
		var execution api.AssetExecution
		if err = json.Unmarshal(target.Execution, &execution); err != nil {
			return err
		}
		for _, iface := range execution.Interfaces {
			interfaces = append(interfaces, api.SecurityInterface{EnvironmentId: target.EnvironmentID, Interface: api.CaptureInterface{
				AssetId: execution.Asset.Id, AssetName: execution.Asset.Name, InterfaceId: iface.Id, NetworkId: iface.NetworkId,
				PortName: iface.PortName, NodeId: target.NodeID, Mac: iface.Mac, Address: iface.Address,
			}})
		}
	}
	nodes, err := q.ListObservedNodes(ctx)
	if err != nil {
		return err
	}
	quotas, err := s.packetQuotas(ctx, interfaces)
	if err != nil {
		return err
	}
	type result struct {
		node string
		err  error
	}
	results := make(chan result, len(nodes))
	var waiting sync.WaitGroup
	for _, node := range nodes {
		waiting.Go(func() {
			results <- result{node.ID, s.nodeSecurity(ctx, node.ID, node.Endpoint, interfaces, quotas[node.ID])}
		})
	}
	waiting.Wait()
	close(results)
	for result := range results {
		if result.err == nil {
			delete(failures, result.node)
		} else if message := result.err.Error(); failures[result.node] != message {
			failures[result.node] = message
			slog.Error("security collection", "node", result.node, "error", message)
		}
	}
	return nil
}

func (s Service) nodeSecurity(parent context.Context, id, endpoint string, all []api.SecurityInterface, budget packetNodeBudget) error {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	q := queries.New(s.Pool)
	cursor, err := q.GetSecurityCursor(ctx, id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	environments := map[string]bool{}
	for _, iface := range all {
		if iface.Interface.NodeId == id {
			environments[iface.EnvironmentId] = true
		}
	}
	request := api.NodeSecurityRequest{Cursor: cursor, Interfaces: []api.SecurityInterface{}, PacketQuotas: &budget.requested}
	for _, iface := range all {
		if environments[iface.EnvironmentId] {
			request.Interfaces = append(request.Interfaces, iface)
		}
	}
	for {
		var observation api.NodeSecurityObservation
		if err = s.Client.Do(ctx, http.MethodPost, endpoint, "/node/v1/security", request, &observation); err != nil {
			// Keep the committed cursor; a failed request must not discard unread EVE data.
			message := err.Error()
			observation = api.NodeSecurityObservation{Cursor: cursor, Sensor: api.SecuritySensor{NodeId: id, Status: "unavailable", CheckedAt: time.Now().UTC(), Error: &message}}
		}
		if observation.Sensor.NodeId != id {
			return fmt.Errorf("security sensor identity mismatch")
		}
		for _, alert := range observation.Alerts {
			if alert.NodeId != id {
				return fmt.Errorf("security alert outside assigned environments")
			}
		}
		if len(observation.Alerts) > 0 && observation.Cursor == cursor {
			return fmt.Errorf("security batch did not advance its cursor")
		}
		if observation.PacketRetention != nil {
			for environment, info := range *observation.PacketRetention {
				quota, exists := budget.reserved[environment]
				if !exists || info.LimitBytes > quota || info.UsedBytes > info.LimitBytes || info.UsedBytes < 0 {
					return fmt.Errorf("节点报文留存额度不符合分配")
				}
			}
		}
		if err = s.commitSecurity(ctx, id, cursor, observation); err != nil {
			return err
		}
		if !observation.HasMore {
			return nil
		}
		cursor, request.Cursor = observation.Cursor, observation.Cursor
	}
}

func (s Service) commitSecurity(ctx context.Context, id, cursor string, observation api.NodeSecurityObservation) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := queries.New(tx)
	alerts := observation.Alerts
	observation.Alerts = nil
	raw, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	changed, err := q.SaveSecurityObservation(ctx, queries.SaveSecurityObservationParams{NodeID: id, Cursor: observation.Cursor, Observation: raw, ExpectedCursor: cursor})
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("安全观测游标已变化")
	}
	if observation.PacketRetention != nil {
		for environment, info := range *observation.PacketRetention {
			data, encodeErr := json.Marshal(info)
			if encodeErr != nil {
				return encodeErr
			}
			if err = q.SavePacketNode(ctx, queries.SavePacketNodeParams{EnvironmentID: environment, NodeID: id, QuotaBytes: info.LimitBytes, Observation: data}); err != nil {
				return err
			}
		}
	}
	if len(alerts) > 0 {
		raw, err := json.Marshal(alerts)
		if err != nil {
			return err
		}
		if err = q.ImportSecurityAlerts(ctx, raw); err != nil {
			return err
		}
		affected := map[string]bool{}
		for _, alert := range alerts {
			affected[alert.EnvironmentId] = true
		}
		for environment := range affected {
			if _, err = q.AddEvent(ctx, queries.AddEventParams{EnvironmentID: &environment, Kind: "security.changed", Payload: []byte(`{}`)}); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}
