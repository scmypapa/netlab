package operation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/jackc/pgx/v5"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
)

func reserveExternal(ctx context.Context, q *queries.Queries, environment string, spec api.EnvironmentSpec, nodes map[string]api.NodeInfo) error {
	for _, nw := range spec.Networks {
		if nw.External == nil {
			continue
		}
		a := nw.External
		info, exists := nodes[a.NodeId]
		if !exists || info.ExternalInterfaces == nil {
			return fmt.Errorf("网段 %s 的外部节点不存在", nw.Name)
		}
		if !slices.ContainsFunc(*info.ExternalInterfaces, func(item api.ExternalInterface) bool {
			return item.Name == a.Interface && item.Available && *nw.Mtu <= item.Mtu
		}) {
			return fmt.Errorf("外部接口 %s 不可用或 MTU 不足", a.Interface)
		}
		vlan := 0
		if a.Vlan != nil {
			vlan = *a.Vlan
		}
		_, err := q.ReserveExternalNetwork(ctx, queries.ReserveExternalNetworkParams{NodeID: a.NodeId, Interface: a.Interface, Vlan: int32(vlan), EnvironmentID: environment, NetworkID: nw.Id})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("外部接口 %s 的 VLAN %d 已由其他环境占用", a.Interface, vlan)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Leases include both sides of a pending switch. They are released only after
// every involved node has applied the final attachment set.
func (w Worker) external(ctx context.Context, op *queries.Operation, specs ...api.EnvironmentSpec) error {
	leases, err := w.Queries.ListExternalNetworks(ctx, *op.EnvironmentID)
	if err != nil {
		return fmt.Errorf("%w: %v", errPersistence, err)
	}
	byNode := map[string][]api.ExternalAttachment{}
	for _, lease := range leases {
		byNode[lease.NodeID] = []api.ExternalAttachment{}
	}
	for _, spec := range specs {
		for _, nw := range spec.Networks {
			if nw.External == nil {
				continue
			}
			a := *nw.External
			if !slices.ContainsFunc(byNode[a.NodeId], func(other api.ExternalAttachment) bool { return a.Key() == other.Key() }) {
				byNode[a.NodeId] = append(byNode[a.NodeId], a)
			}
		}
	}
	ids := make([]string, 0, len(byNode))
	for id := range byNode {
		ids = append(ids, id)
	}
	nodes, err := w.Queries.GetNodeEndpoints(ctx, ids)
	if err != nil {
		return fmt.Errorf("%w: %v", errPersistence, err)
	}
	if len(nodes) != len(ids) {
		return fmt.Errorf("外部网络节点不存在")
	}
	errorsByNode := make([]error, len(nodes))
	var group sync.WaitGroup
	for i, node := range nodes {
		group.Add(1)
		go func() {
			defer group.Done()
			attachments := byNode[node.ID]
			result, err := w.Client.Execute(ctx, node.Endpoint, api.NodePlan{OperationId: op.ID, EnvironmentId: *op.EnvironmentID, Phase: api.NodePlanPhaseExternalAttachments, Spec: api.EnvironmentSpec{}, Assets: []api.AssetExecution{}, Attachments: &attachments})
			if err == nil && result.Error != nil {
				err = errors.New(*result.Error)
			}
			if err != nil {
				errorsByNode[i] = fmt.Errorf("%s: %w", node.ID, err)
			}
		}()
	}
	group.Wait()
	return errors.Join(errorsByNode...)
}

func releaseExternal(ctx context.Context, q *queries.Queries, environment string, spec api.EnvironmentSpec) error {
	retained := []string{}
	for _, nw := range spec.Networks {
		if nw.External != nil {
			retained = append(retained, nw.External.Key())
		}
	}
	return q.ReleaseExternalNetworks(ctx, queries.ReleaseExternalNetworksParams{EnvironmentID: environment, Retained: retained})
}
