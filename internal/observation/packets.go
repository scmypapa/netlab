package observation

import (
	"context"
	"encoding/json"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/capture"
)

// Grants partition one environment's capacity across current and retained nodes.
// A decrease is committed only after the node has applied and acknowledged it.
type packetNodeBudget struct{ requested, reserved map[string]int64 }

func (s Service) packetQuotas(ctx context.Context, interfaces []api.SecurityInterface) (map[string]packetNodeBudget, error) {
	q := queries.New(s.Pool)
	active := map[string]map[string]bool{}
	for _, iface := range interfaces {
		if active[iface.EnvironmentId] == nil {
			active[iface.EnvironmentId] = map[string]bool{}
		}
		if active[iface.EnvironmentId][iface.Interface.NodeId] {
			continue
		}
		active[iface.EnvironmentId][iface.Interface.NodeId] = true
		if err := q.EnsurePacketNode(ctx, queries.EnsurePacketNodeParams{EnvironmentID: iface.EnvironmentId, NodeID: iface.Interface.NodeId}); err != nil {
			return nil, err
		}
	}
	rows, err := q.ListPacketNodes(ctx)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	retained := map[string]bool{}
	for _, row := range rows {
		var info api.PacketRetention
		if err = json.Unmarshal(row.Observation, &info); err != nil {
			return nil, err
		}
		needed := active[row.EnvironmentID][row.NodeID] || info.UsedBytes > 0
		retained[row.EnvironmentID+"/"+row.NodeID] = needed
		if needed {
			counts[row.EnvironmentID]++
		}
	}
	desired := map[string]map[string]int64{}
	for _, row := range rows {
		limit := int64(0)
		if retained[row.EnvironmentID+"/"+row.NodeID] {
			limit = capture.PacketBudget / int64(counts[row.EnvironmentID])
		}
		if desired[row.NodeID] == nil {
			desired[row.NodeID] = map[string]int64{}
		}
		desired[row.NodeID][row.EnvironmentID] = limit
		if limit > row.QuotaBytes {
			if err = q.GrantPacketQuota(ctx, queries.GrantPacketQuotaParams{EnvironmentID: row.EnvironmentID, NodeID: row.NodeID, QuotaBytes: limit}); err != nil {
				return nil, err
			}
		}
	}
	rows, err = q.ListPacketNodes(ctx)
	if err != nil {
		return nil, err
	}
	result := map[string]packetNodeBudget{}
	for _, row := range rows {
		budget, exists := result[row.NodeID]
		if !exists {
			budget = packetNodeBudget{requested: map[string]int64{}, reserved: map[string]int64{}}
		}
		budget.requested[row.EnvironmentID] = min(row.QuotaBytes, desired[row.NodeID][row.EnvironmentID])
		budget.reserved[row.EnvironmentID] = row.QuotaBytes
		result[row.NodeID] = budget
	}
	return result, nil
}
