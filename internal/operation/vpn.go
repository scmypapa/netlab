package operation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"

	"github.com/jackc/pgx/v5"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/environment"
)

func (w Worker) vpnOperation(ctx context.Context, op *queries.Operation, p *Payload) error {
	if op.Phase == "complete" {
		return nil
	}
	if op.Phase == "queued" {
		row, err := w.Queries.GetEnvironment(ctx, *op.EnvironmentID)
		if err != nil {
			return err
		}
		if row.Revision != op.ExpectedRevision {
			return errors.Join(environment.ErrConflict, w.status(ctx, op, p, environment.ErrConflict))
		}
		if err = json.Unmarshal(row.AppliedSpec, &p.Spec); err != nil {
			return err
		}
		p.VPNBefore, p.VPNPlan, err = w.prepareVPN(ctx, op, p.Spec, p.VPNChange)
		if err != nil {
			return errors.Join(err, w.status(ctx, op, p, err))
		}
		if err = w.phase(ctx, op, p, "vpn"); err != nil {
			return err
		}
	}
	result, err := w.callVPN(ctx, op, p.Spec, *p.VPNPlan)
	if err == nil {
		p.VPNResult = result
		err = w.finishVPN(ctx, op, p)
	}
	if ctx.Err() != nil || errors.Is(err, errPersistence) {
		return err
	}
	if err != nil {
		_, restoreErr := w.callVPN(ctx, op, p.Spec, *p.VPNBefore)
		return errors.Join(err, restoreErr, w.status(ctx, op, p, err))
	}
	return nil
}

func (w Worker) prepareVPN(ctx context.Context, op *queries.Operation, spec api.EnvironmentSpec, change *environment.VPNChange) (*api.NodeVPNPlan, *api.NodeVPNPlan, error) {
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)
	q := w.Queries.WithTx(tx)
	row, err := q.LockEnvironment(ctx, *op.EnvironmentID)
	if err != nil {
		return nil, nil, err
	}
	rows, err := q.ListVPNAccess(ctx, row.ID)
	if err != nil {
		return nil, nil, err
	}
	before, next := &api.NodeVPNPlan{Peers: []api.VPNPeer{}}, &api.NodeVPNPlan{Peers: []api.VPNPeer{}}
	for _, value := range rows {
		var peer api.VPNPeer
		if err = json.Unmarshal(value.Definition, &peer); err != nil {
			return nil, nil, err
		}
		if value.Applied {
			addresses := []string{}
			for _, addr := range value.Addresses {
				addresses = append(addresses, netip.PrefixFrom(addr, addr.BitLen()).String())
			}
			peer.Addresses = &addresses
			before.Peers = append(before.Peers, peer)
		}
		include := value.Applied
		if change != nil && change.ID == value.ID {
			include = !change.Remove
		}
		if include {
			next.Peers = append(next.Peers, peer)
		}
	}
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(73421494)"); err != nil {
		return nil, nil, err
	}
	next.Peers, err = environment.VPNRoutes(ctx, q, row.ID, spec, next.Peers)
	if err != nil {
		return nil, nil, err
	}
	next.Peers = slices.DeleteFunc(next.Peers, func(peer api.VPNPeer) bool { return len(peer.Routes) == 0 })
	port, err := q.GetVPNPort(ctx, row.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		if len(next.Peers) > 0 {
			if row.NetworkNodeID == nil {
				return nil, nil, errors.New("运行环境没有网络节点")
			}
			if err = q.ReserveVPNPort(ctx, queries.ReserveVPNPortParams{NodeID: *row.NetworkNodeID, EnvironmentID: row.ID, OperationID: op.ID}); err != nil {
				return nil, nil, err
			}
		}
	} else if err != nil {
		return nil, nil, err
	} else if port.Port != nil {
		before.ListenPort = int(*port.Port)
		next.ListenPort = before.ListenPort
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return before, next, nil
}

func (w Worker) callVPN(ctx context.Context, op *queries.Operation, spec api.EnvironmentSpec, plan api.NodeVPNPlan) (*api.NodeVPNResult, error) {
	row, err := w.Queries.GetEnvironment(ctx, *op.EnvironmentID)
	if err != nil {
		return nil, err
	}
	if row.NetworkNodeID == nil {
		return nil, errors.New("运行环境没有网络节点")
	}
	endpoints, err := w.Queries.GetNodeEndpoints(ctx, []string{*row.NetworkNodeID})
	if err != nil {
		return nil, err
	}
	if len(endpoints) != 1 {
		return nil, errors.New("运行环境网络节点不存在")
	}
	result, err := w.Client.Execute(ctx, endpoints[0].Endpoint, api.NodePlan{OperationId: op.ID, EnvironmentId: row.ID, Phase: api.NodePlanPhaseVpn, Assets: []api.AssetExecution{}, Spec: spec, Vpn: &plan})
	if err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, errors.New(*result.Error)
	}
	if result.Vpn == nil || len(result.Vpn.Peers) != len(plan.Peers) {
		return nil, errors.New("节点未确认完整 VPN 配置")
	}
	if len(plan.Peers) > 0 && (result.Vpn.ListenPort < 1 || result.Vpn.ListenPort > 65535 || result.Vpn.PublicKey == "" || plan.ListenPort != 0 && result.Vpn.ListenPort != plan.ListenPort) {
		return nil, errors.New("节点 VPN 入口与任务不符")
	}
	expected := map[string]api.VPNPeer{}
	for _, peer := range plan.Peers {
		expected[peer.Id] = peer
	}
	for _, peer := range result.Vpn.Peers {
		target, exists := expected[peer.Id]
		if !exists || len(peer.Addresses) == 0 || target.Addresses != nil && !slices.Equal(*target.Addresses, peer.Addresses) {
			return nil, errors.New("节点 VPN 用户结果与任务不符")
		}
		for _, value := range peer.Addresses {
			prefix, parseErr := netip.ParsePrefix(value)
			if parseErr != nil || prefix.Bits() != prefix.Addr().BitLen() {
				return nil, errors.New("节点 VPN 客户端地址无效")
			}
		}
		delete(expected, peer.Id)
	}
	return result.Vpn, nil
}

func (w Worker) finishVPN(ctx context.Context, op *queries.Operation, p *Payload) error {
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := w.Queries.WithTx(tx)
	if _, err = q.LockEnvironment(ctx, *op.EnvironmentID); err != nil {
		return err
	}
	if err = commitVPN(ctx, q, *op.EnvironmentID, p); err != nil {
		return err
	}
	transactionWorker := w
	transactionWorker.Queries = q
	if err = transactionWorker.status(ctx, op, p, nil); err != nil {
		return err
	}
	if err = transactionWorker.phase(ctx, op, p, "complete"); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: %v", errPersistence, err)
	}
	return nil
}

func commitVPN(ctx context.Context, q *queries.Queries, environmentID string, p *Payload) error {
	next, before, result := p.VPNPlan, p.VPNBefore, p.VPNResult
	confirmed := map[string]bool{}
	for _, peer := range result.Peers {
		confirmed[peer.Id] = true
		addresses := []netip.Addr{}
		for _, value := range peer.Addresses {
			prefix, _ := netip.ParsePrefix(value)
			addresses = append(addresses, prefix.Addr())
		}
		definition := next.Peers[slices.IndexFunc(next.Peers, func(target api.VPNPeer) bool { return target.Id == peer.Id })]
		definition.Addresses = nil
		raw, err := json.Marshal(definition)
		if err != nil {
			return err
		}
		if err = q.ApplyVPNAccess(ctx, queries.ApplyVPNAccessParams{EnvironmentID: environmentID, ID: peer.Id, Definition: raw, Addresses: addresses}); err != nil {
			return err
		}
	}
	for _, peer := range before.Peers {
		if !confirmed[peer.Id] {
			if err := q.DeleteVPNAccess(ctx, queries.DeleteVPNAccessParams{EnvironmentID: environmentID, ID: peer.Id}); err != nil {
				return err
			}
		}
	}
	if p.VPNChange != nil && p.VPNChange.Remove {
		if err := q.DeleteVPNAccess(ctx, queries.DeleteVPNAccessParams{EnvironmentID: environmentID, ID: p.VPNChange.ID}); err != nil {
			return err
		}
	}
	if len(next.Peers) > 0 {
		port, mtu := int32(result.ListenPort), int32(result.Mtu)
		if err := q.ApplyVPNPort(ctx, queries.ApplyVPNPortParams{EnvironmentID: environmentID, Port: &port}); err != nil {
			return err
		}
		if err := q.SetVPNGateway(ctx, queries.SetVPNGatewayParams{ID: environmentID, VpnPublicKey: &result.PublicKey, VpnMtu: &mtu}); err != nil {
			return err
		}
	} else {
		if err := q.DeleteVPNPort(ctx, environmentID); err != nil {
			return err
		}
		if err := q.SetVPNGateway(ctx, queries.SetVPNGatewayParams{ID: environmentID}); err != nil {
			return err
		}
	}
	return q.DeleteUnusedVPNAliases(ctx, environmentID)
}

func (w Worker) syncVPN(ctx context.Context, op *queries.Operation, p *Payload) error {
	rows, err := w.Queries.ListVPNAccess(ctx, *op.EnvironmentID)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(rows, func(row queries.ListVPNAccessRow) bool { return row.Applied }) {
		return nil
	}
	if p.VPNPlan == nil {
		p.VPNBefore, p.VPNPlan, err = w.prepareVPN(ctx, op, p.Spec, nil)
		if err != nil {
			return err
		}
		if err = w.phase(ctx, op, p, op.Phase); err != nil {
			return err
		}
	}
	p.VPNResult, err = w.callVPN(ctx, op, p.Spec, *p.VPNPlan)
	return err
}
