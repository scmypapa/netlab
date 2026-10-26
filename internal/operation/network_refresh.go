package operation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/environment"
)

func (w Worker) currentTargets(ctx context.Context, id string) ([]Target, error) {
	assets, err := w.Queries.ListRuntimeAssets(ctx, id)
	if err != nil {
		return nil, err
	}
	result := make([]Target, 0, len(assets))
	for _, asset := range assets {
		if !asset.Current {
			continue
		}
		var execution api.AssetExecution
		if err := json.Unmarshal(asset.Execution, &execution); err != nil {
			return nil, err
		}
		result = append(result, Target{NodeID: asset.NodeID, Execution: execution, State: asset.State})
	}
	return result, nil
}

// A routing-format upgrade only reapplies network resources. It never enters
// guest placement, image preparation or the instance stop/start path.
func (w Worker) refreshNetwork(ctx context.Context, op *queries.Operation, p *Payload) error {
	if op.Phase == "queued" {
		row, err := w.Queries.GetEnvironment(ctx, *op.EnvironmentID)
		if err != nil {
			return err
		}
		if row.Status == "destroyed" || row.AppliedSpec == nil {
			return w.phase(ctx, op, p, "complete")
		}
		if err = json.Unmarshal(row.AppliedSpec, &p.Spec); err != nil {
			return err
		}
		before := p.Spec
		p.BeforeSpec = &before
		p.BeforeStatus = row.Status
		p.Spec, err = environment.CompleteRouting(p.Spec)
		if err != nil {
			return err
		}
		if row.NetworkNodeID == nil {
			return errors.New("环境网络所属节点不存在")
		}
		p.Owner = &Target{NodeID: *row.NetworkNodeID}
		nodes, err := w.Queries.ListNodes(ctx)
		if err != nil {
			return err
		}
		p.ExternalChassis = map[string]string{}
		for _, node := range nodes {
			var info api.NodeInfo
			if err := json.Unmarshal(node.Info, &info); err != nil {
				return err
			}
			if info.NetworkChassis != nil {
				p.ExternalChassis[node.ID] = *info.NetworkChassis
			}
		}
		p.Unchanged, err = w.currentTargets(ctx, row.ID)
		if err != nil {
			return err
		}
		if err := w.loadServices(ctx, row, p); err != nil {
			return err
		}
		p.Bindings = slices.Clone(p.BeforeBindings)
		if err := w.phase(ctx, op, p, "network"); err != nil {
			return err
		}
	}
	for op.Phase != "complete" {
		var err error
		next := ""
		switch op.Phase {
		case "network":
			err = w.network(ctx, op, p, false)
			next = "services"
		case "services":
			p.Bindings, err = w.serviceRules(ctx, op, p, p.Spec, p.Bindings)
			next = "vpn"
		case "vpn":
			err = w.syncVPN(ctx, op, p)
			next = "commit"
		case "commit":
			tx, beginErr := w.Pool.Begin(ctx)
			if beginErr != nil {
				return beginErr
			}
			defer tx.Rollback(ctx)
			q := w.Queries.WithTx(tx)
			raw, marshalErr := json.Marshal(p.Spec)
			err = marshalErr
			if err == nil {
				err = q.RefreshEnvironmentNetwork(ctx, queries.RefreshEnvironmentNetworkParams{ID: *op.EnvironmentID, AppliedSpec: raw})
			}
			if err == nil && p.Gateway != nil {
				err = commitServices(ctx, q, *op.EnvironmentID, p.Bindings)
			}
			if err == nil && p.VPNResult != nil {
				err = commitVPN(ctx, q, *op.EnvironmentID, p)
			}
			if err != nil {
				return err
			}
			tw := w
			tw.Queries = q
			if err = tw.phase(ctx, op, p, "complete"); err != nil {
				return err
			}
			if err = tx.Commit(ctx); err != nil {
				return fmt.Errorf("%w: %v", errPersistence, err)
			}
			return nil
		default:
			return fmt.Errorf("未知网络重应用阶段：%s", op.Phase)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", op.Phase, err)
		}
		if err := w.phase(ctx, op, p, next); err != nil {
			return err
		}
	}
	return nil
}
