package operation

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/environment"
)

// A task's persisted phase remains its progress authority during an upgrade.
// Only a task already past its network boundary needs that boundary replayed.
func (w Worker) upgradeRouting(ctx context.Context, op *queries.Operation, p *Payload) error {
	if op.EnvironmentID == nil || op.Kind == "refresh-network" || op.ScopeKind != "environment" {
		return nil
	}
	lifecycle := op.Kind == "start" || op.Kind == "change" || op.Kind == "rebuild" || restoresData(op.Kind)
	if !lifecycle && op.Kind != "move-network" && op.Kind != "vpn-create" && op.Kind != "vpn-revoke" {
		return nil
	}
	changed := false
	specs := []*api.EnvironmentSpec{&p.Spec, p.BeforeSpec}
	if p.Recovery != nil {
		specs = append(specs, &p.Recovery.Spec)
	}
	if lifecycle && op.Phase == "rollback" {
		specs = []*api.EnvironmentSpec{p.BeforeSpec}
	}
	for _, spec := range specs {
		if spec == nil || len(spec.Networks) == 0 {
			continue
		}
		complete, err := environment.CompleteRouting(*spec)
		if err != nil {
			return err
		}
		changed = changed || !reflect.DeepEqual(*spec, complete)
		*spec = complete
	}
	for _, pair := range []struct {
		bindings []api.NodeServiceBinding
		spec     *api.EnvironmentSpec
	}{{p.Bindings, &p.Spec}, {p.BeforeBindings, p.BeforeSpec}} {
		if pair.spec == nil {
			continue
		}
		interfaces := map[[2]string]string{}
		for _, asset := range pair.spec.Assets {
			for _, iface := range asset.Interfaces {
				interfaces[[2]string{asset.Id, iface.Id}] = iface.NetworkId
			}
		}
		for i := range pair.bindings {
			binding := &pair.bindings[i]
			if binding.NetworkId == "" {
				binding.NetworkId = interfaces[[2]string{binding.AssetId, binding.InterfaceId}]
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	if !lifecycle {
		return w.phase(ctx, op, p, op.Phase)
	}
	switch op.Phase {
	case "update", "activate", "policies", "verify", "services", "vpn", "commit", "cleanup":
		if len(desiredTargets(p)) == 0 && len(p.Spec.Assets) > 0 {
			var err error
			p.Unchanged, err = w.currentTargets(ctx, *op.EnvironmentID)
			if err != nil {
				return err
			}
		}
		if err := w.network(ctx, op, p, false); err != nil {
			return err
		}
		if op.Phase == "vpn" || op.Phase == "commit" || op.Phase == "cleanup" {
			var err error
			p.Bindings, err = w.serviceRules(ctx, op, p, p.Spec, p.Bindings)
			if err != nil {
				return err
			}
		}
	}
	if !p.Committed || op.Phase != "cleanup" {
		return w.phase(ctx, op, p, op.Phase)
	}
	// A committed task will not visit commit again when cleanup resumes.
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", errPersistence, err)
	}
	defer tx.Rollback(ctx)
	q := w.Queries.WithTx(tx)
	raw, err := json.Marshal(p.Spec)
	if err == nil {
		err = q.RefreshEnvironmentNetwork(ctx, queries.RefreshEnvironmentNetworkParams{ID: *op.EnvironmentID, AppliedSpec: raw})
	}
	if err != nil {
		return fmt.Errorf("%w: %v", errPersistence, err)
	}
	tw := w
	tw.Queries = q
	if err = tw.phase(ctx, op, p, op.Phase); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: %v", errPersistence, err)
	}
	return nil
}
