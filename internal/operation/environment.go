package operation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/environment"
)

func (w Worker) environment(ctx context.Context, op *queries.Operation, p *Payload) error {
	if op.Phase == "rolled-back" {
		return errors.New(*p.Failure)
	}
	if op.Phase == "queued" {
		if err := w.plan(ctx, op, p); err != nil {
			return errors.Join(err, w.status(ctx, op, p, err))
		}
	}
	if op.Phase == "rollback" {
		return w.rollback(ctx, op, p)
	}
	for op.Phase != "complete" {
		var err error
		next := ""
		switch op.Phase {
		case "network":
			err = w.network(ctx, op, p, false)
			next = "update"
		case "prepare":
			_, err = w.batch(ctx, op, p, api.NodePlanPhasePrepare, p.Targets)
			next = "quiesce"
		case "quiesce":
			affected := slices.Clone(p.Old)
			for _, t := range p.Updates {
				old := findInstance(p.Before, t.Execution.InstanceId)
				if environment.RequiresStop(t.Execution.Template.Kind, old.Execution.Asset, t.Execution.Asset) || !reflect.DeepEqual(old.Execution.Interfaces, t.Execution.Interfaces) {
					affected = append(affected, old)
				}
			}
			// Uncreated leftovers are handled by destroy; only defined current instances need stopping.
			affected = slices.DeleteFunc(affected, func(t Target) bool { return findInstance(p.Before, t.Execution.InstanceId).Execution.InstanceId == "" })
			err = w.quiesceServices(ctx, op, p, affected)
			if err == nil {
				_, err = w.batch(ctx, op, p, api.NodePlanPhaseStop, affected)
			}
			next = "network"
		case "update":
			_, err = w.batch(ctx, op, p, api.NodePlanPhaseUpdate, p.Updates)
			next = "activate"
		case "activate":
			targets := append(slices.Clone(p.Targets), p.Updates...)
			if op.Kind == "start" {
				for _, t := range p.Unchanged {
					if op.AssetID == nil || t.Execution.Asset.Id == *op.AssetID {
						targets = append(targets, t)
					}
				}
			}
			err = w.activate(ctx, op, p, targets)
			next = "policies"
		case "policies":
			_, err = w.batch(ctx, op, p, api.NodePlanPhasePolicies, desiredTargets(p))
			next = "verify"
		case "verify":
			desired := desiredTargets(p)
			var results []api.ExecutionResult
			results, err = w.batch(ctx, op, p, api.NodePlanPhaseInspect, desired)
			if err == nil {
				for _, r := range results {
					t := findInstance(desired, r.InstanceId)
					if !matches(r.State, t.State) {
						err = errors.Join(err, fmt.Errorf("资产 %s 的实际状态为 %s，目标为 %s", t.Execution.Asset.Name, r.State, t.State))
					}
				}
			}
			next = "vpn"
			if p.Gateway != nil {
				next = "services"
			}
		case "services":
			p.Bindings, err = w.serviceRules(ctx, op, p, p.Spec, p.Bindings)
			next = "vpn"
		case "vpn":
			err = w.syncVPN(ctx, op, p)
			next = "commit"
		case "commit":
			err = w.commit(ctx, op, p)
			next = "cleanup"
		case "cleanup":
			err = w.cleanup(ctx, op, p)
			if err == nil {
				err = w.status(ctx, op, p, nil)
			}
			next = "complete"
		case "control":
			phase := api.NodePlanPhase(op.Kind)
			if op.Kind == "reboot" {
				phase = api.NodePlanPhaseStop
			}
			var results []api.ExecutionResult
			results, err = w.batch(ctx, op, p, phase, p.Targets)
			if err == nil {
				expected := map[string]string{"stop": "stopped", "force-stop": "stopped", "suspend": "suspended", "resume": "running", "reboot": "stopped"}[op.Kind]
				for _, r := range results {
					if !matches(r.State, expected) {
						err = errors.Join(err, fmt.Errorf("资产 %s 未完成 %s，实际状态 %s", r.AssetId, op.Kind, r.State))
					}
				}
			}
			if op.Kind == "reboot" {
				next = "restart"
			} else {
				next = "settle"
			}
		case "restart":
			for i := range p.Targets {
				p.Targets[i].State = "running"
			}
			err = w.activate(ctx, op, p, p.Targets)
			next = "settle"
		case "settle":
			err = w.status(ctx, op, p, nil)
			next = "complete"
		case "remove-services":
			_, err = w.serviceRules(ctx, op, p, p.Spec, nil)
			if err == nil {
				err = w.Queries.ReleaseServicePorts(ctx, *op.EnvironmentID)
				if err != nil {
					err = fmt.Errorf("%w: %v", errPersistence, err)
				}
			}
			next = "destroy"
		case "destroy":
			targets := append(slices.Clone(p.Targets), p.Old...)
			var results []api.ExecutionResult
			results, err = w.batch(ctx, op, p, api.NodePlanPhaseDestroy, targets)
			err = errors.Join(err, w.releaseDestroyed(ctx, results))
			next = "remove-network"
		case "remove-network":
			err = w.network(ctx, op, p, true)
			if err == nil {
				err = releaseExternal(ctx, w.Queries, *op.EnvironmentID, api.EnvironmentSpec{})
			}
			if err == nil {
				err = w.Queries.ReleaseAllAccessPorts(ctx, *op.EnvironmentID)
			}
			if err == nil {
				err = w.Queries.DeleteEnvironmentVPN(ctx, *op.EnvironmentID)
			}
			if err == nil {
				err = w.Queries.SetGatewayAddress(ctx, queries.SetGatewayAddressParams{EnvironmentID: *op.EnvironmentID})
			}
			next = "destroyed"
		case "destroyed":
			err = w.Queries.SetEnvironmentState(ctx, queries.SetEnvironmentStateParams{ID: *op.EnvironmentID, Status: "destroyed"})
			next = "complete"
		default:
			return fmt.Errorf("未知执行阶段 %s", op.Phase)
		}
		if ctx.Err() != nil || errors.Is(err, errPersistence) {
			return errors.Join(err, ctx.Err())
		}
		if err != nil {
			detail := fmt.Errorf("%s: %w", op.Phase, err)
			if p.Committed || op.Phase == "remove-services" || op.Phase == "destroy" || op.Phase == "remove-network" || op.Phase == "control" || op.Phase == "restart" || op.Phase == "settle" {
				return errors.Join(detail, w.status(ctx, op, p, detail))
			}
			message := detail.Error()
			p.Failure = &message
			if saveErr := w.phase(ctx, op, p, "rollback"); saveErr != nil {
				return errors.Join(detail, saveErr)
			}
			return w.rollback(ctx, op, p)
		}
		if err = w.phase(ctx, op, p, next); err != nil {
			return err
		}
	}
	return nil
}

func desiredTargets(p *Payload) []Target {
	r := make([]Target, 0, len(p.Targets)+len(p.Updates)+len(p.Unchanged))
	r = append(r, p.Targets...)
	r = append(r, p.Updates...)
	return append(r, p.Unchanged...)
}

func matches(actual, expected string) bool {
	if expected == "stopped" {
		return actual == "stopped" || actual == "prepared" || actual == "created"
	}
	return actual == expected
}

func (w Worker) activate(ctx context.Context, op *queries.Operation, p *Payload, targets []Target) error {
	active := slices.DeleteFunc(slices.Clone(targets), func(t Target) bool { return t.State != "running" && t.State != "suspended" })
	results, err := w.batch(ctx, op, p, api.NodePlanPhaseStart, active)
	if err != nil {
		return err
	}
	resume := []Target{}
	for _, r := range results {
		t := findInstance(active, r.InstanceId)
		if r.State == "suspended" && t.State == "running" {
			resume = append(resume, t)
		}
	}
	if _, err = w.batch(ctx, op, p, api.NodePlanPhaseResume, resume); err != nil {
		return err
	}
	suspended := slices.DeleteFunc(active, func(t Target) bool { return t.State != "suspended" })
	_, err = w.batch(ctx, op, p, api.NodePlanPhaseSuspend, suspended)
	return err
}

func (w Worker) commit(ctx context.Context, op *queries.Operation, p *Payload) error {
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := w.Queries.WithTx(tx)
	row, err := q.LockEnvironment(ctx, *op.EnvironmentID)
	if err != nil {
		return err
	}
	if row.Revision != op.ExpectedRevision {
		return fmt.Errorf("运行修订已变化")
	}
	if p.BeforeSpec == nil || !environment.ServiceOnly(*p.BeforeSpec, p.Spec) || op.Kind != "change" {
		ids := []string{}
		for _, t := range p.Old {
			ids = append(ids, t.Execution.Asset.Id)
		}
		if err = q.ClearCurrentAssets(ctx, queries.ClearCurrentAssetsParams{EnvironmentID: row.ID, Column2: ids}); err != nil {
			return err
		}
		instances := []string{}
		for _, t := range p.Targets {
			instances = append(instances, t.Execution.InstanceId)
		}
		if err = q.MakeCurrentAssets(ctx, instances); err != nil {
			return err
		}
		raw, err := resourceRecords(row.ID, desiredTargets(p))
		if err != nil {
			return err
		}
		if err = q.UpdateExecutions(ctx, raw); err != nil {
			return err
		}
	}
	state, err := runtimeState(ctx, q, row.ID)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(p.Spec)
	if err != nil {
		return err
	}
	if op.Kind == "start" && p.BeforeSpec != nil && len(p.Targets) == 0 && len(p.Updates) == 0 {
		err = q.SetEnvironmentState(ctx, queries.SetEnvironmentStateParams{ID: row.ID, Status: state})
	} else {
		err = q.CommitEnvironment(ctx, queries.CommitEnvironmentParams{ID: row.ID, AppliedSpec: raw, Status: state})
	}
	if err != nil {
		return err
	}
	if p.Gateway != nil {
		if err = commitServices(ctx, q, row.ID, p.Bindings); err != nil {
			return err
		}
	}
	if p.VPNResult != nil {
		if err = commitVPN(ctx, q, row.ID, p); err != nil {
			return err
		}
	}
	p.Committed = true
	transactionWorker := w
	transactionWorker.Queries = q
	if err = transactionWorker.phase(ctx, op, p, "cleanup"); err != nil {
		p.Committed = false
		return err
	}
	// Configuration, instance ownership and the next phase share one commit.
	if err = tx.Commit(ctx); err != nil {
		p.Committed = false
		return fmt.Errorf("%w: %v", errPersistence, err)
	}
	return nil
}

func (w Worker) cleanup(ctx context.Context, op *queries.Operation, p *Payload) error {
	if op.Kind == "change" && p.BeforeSpec != nil && environment.ServiceOnly(*p.BeforeSpec, p.Spec) {
		return nil
	}
	if err := w.external(ctx, op, p.Spec); err != nil {
		return err
	}
	if err := releaseExternal(ctx, w.Queries, *op.EnvironmentID, p.Spec); err != nil {
		return fmt.Errorf("%w: %v", errPersistence, err)
	}
	results, err := w.batch(ctx, op, p, api.NodePlanPhaseDestroy, p.Old)
	if releaseErr := w.releaseDestroyed(ctx, results); releaseErr != nil {
		err = errors.Join(err, releaseErr)
	}
	if err != nil {
		return err
	}
	_, err = w.batch(ctx, op, p, api.NodePlanPhaseCleanupVolumes, removedVolumes(p.Before, p.Updates))
	return err
}

func removedVolumes(beforeTargets, afterTargets []Target) []Target {
	removed := []Target{}
	for _, t := range afterTargets {
		before := findInstance(beforeTargets, t.Execution.InstanceId)
		if before.Execution.Asset.Volumes == nil {
			continue
		}
		volumes := []api.Volume{}
		for _, v := range *before.Execution.Asset.Volumes {
			found := false
			if t.Execution.Asset.Volumes != nil {
				for _, next := range *t.Execution.Asset.Volumes {
					if v.Id == next.Id {
						found = true
						break
					}
				}
			}
			if !found {
				volumes = append(volumes, v)
			}
		}
		if len(volumes) > 0 {
			t.Execution.Asset.Volumes = &volumes
			removed = append(removed, t)
		}
	}
	return removed
}

func (w Worker) releaseDestroyed(ctx context.Context, results []api.ExecutionResult) error {
	ids := []string{}
	for _, r := range results {
		if r.Error == nil && (r.State == "destroyed" || r.State == "absent") {
			ids = append(ids, r.InstanceId)
		}
	}
	return w.Queries.ReleaseAssets(ctx, ids)
}

func (w Worker) rollback(ctx context.Context, op *queries.Operation, p *Payload) error {
	initial := errors.New(*p.Failure)
	if op.Kind == "change" && p.BeforeSpec != nil && environment.ServiceOnly(*p.BeforeSpec, p.Spec) {
		if _, err := w.serviceRules(ctx, op, p, *p.BeforeSpec, p.BeforeBindings); err != nil {
			return errors.Join(initial, err)
		}
		tx, err := w.Pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("%w: %v", errPersistence, err)
		}
		defer tx.Rollback(ctx)
		q := w.Queries.WithTx(tx)
		if _, err = q.LockEnvironment(ctx, *op.EnvironmentID); err != nil {
			return fmt.Errorf("%w: %v", errPersistence, err)
		}
		if err = commitServices(ctx, q, *op.EnvironmentID, p.BeforeBindings); err != nil {
			return fmt.Errorf("%w: %v", errPersistence, err)
		}
		transactionWorker := w
		transactionWorker.Queries = q
		if err = transactionWorker.status(ctx, op, p, initial); err != nil {
			return fmt.Errorf("%w: %v", errPersistence, err)
		}
		if err = transactionWorker.phase(ctx, op, p, "rolled-back"); err != nil {
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("%w: %v", errPersistence, err)
		}
		return initial
	}
	var serviceErr error
	if p.BeforeSpec != nil {
		_, serviceErr = w.serviceRules(ctx, op, p, p.Spec, nil)
	}
	results, err := w.batch(ctx, op, p, api.NodePlanPhaseDestroy, p.Targets)
	err = errors.Join(serviceErr, err, w.releaseDestroyed(ctx, results))
	before := slices.Clone(p.Before)
	var updateError error
	if len(p.Updates) > 0 {
		_, updateError = w.batch(ctx, op, p, api.NodePlanPhaseStop, p.Updates)
		observed, inspectErr := w.batch(ctx, op, p, api.NodePlanPhaseInspect, p.Updates)
		updateError = errors.Join(updateError, inspectErr)
		original := []Target{}
		for _, t := range p.Updates {
			old := findInstance(before, t.Execution.InstanceId)
			if old.Execution.Asset.Volumes != nil {
				volumes := slices.Clone(*old.Execution.Asset.Volumes)
				old.Execution.Asset.Volumes = &volumes
			}
			for _, r := range observed {
				if r.InstanceId != t.Execution.InstanceId || r.Execution == nil {
					continue
				}
				// A completed expansion stays expanded; its data cannot be rolled back by shrinking.
				old.Execution.Asset.Resources.DiskGiB = max(old.Execution.Asset.Resources.DiskGiB, r.Execution.Asset.Resources.DiskGiB)
				if old.Execution.Asset.Volumes != nil && r.Execution.Asset.Volumes != nil {
					for i := range *old.Execution.Asset.Volumes {
						for _, actual := range *r.Execution.Asset.Volumes {
							if (*old.Execution.Asset.Volumes)[i].Id == actual.Id {
								(*old.Execution.Asset.Volumes)[i].SizeGiB = max((*old.Execution.Asset.Volumes)[i].SizeGiB, actual.SizeGiB)
							}
						}
					}
				}
			}
			original = append(original, old)
			for i := range before {
				if before[i].Execution.InstanceId == old.Execution.InstanceId {
					before[i] = old
				}
			}
		}
		_, restoreErr := w.batch(ctx, op, p, api.NodePlanPhaseUpdate, original)
		updateError = errors.Join(updateError, restoreErr)
	}
	err = errors.Join(err, updateError)
	if errors.Is(err, errPersistence) || ctx.Err() != nil {
		return errors.Join(initial, err, ctx.Err())
	}
	var restored *api.EnvironmentSpec
	if p.BeforeSpec != nil {
		old := *p.BeforeSpec
		old.Assets = slices.Clone(old.Assets)
		for i := range old.Assets {
			for _, t := range before {
				if old.Assets[i].Id == t.Execution.Asset.Id {
					old.Assets[i] = t.Execution.Asset
				}
			}
		}
		oldPlan := *p
		oldPlan.Spec = old
		oldPlan.Bindings = p.BeforeBindings
		oldPlan.Targets = before
		oldPlan.Updates = nil
		oldPlan.Unchanged = nil
		err = errors.Join(err, w.network(ctx, op, &oldPlan, false))
		if p.VPNBefore != nil {
			_, vpnErr := w.callVPN(ctx, op, old, *p.VPNBefore)
			err = errors.Join(err, vpnErr)
		}
		err = errors.Join(err, w.activate(ctx, op, p, before))
		_, policyErr := w.batch(ctx, op, p, api.NodePlanPhasePolicies, before)
		err = errors.Join(err, policyErr)
		observed, inspectErr := w.batch(ctx, op, p, api.NodePlanPhaseInspect, before)
		err = errors.Join(err, inspectErr)
		for _, r := range observed {
			t := findInstance(before, r.InstanceId)
			if !matches(r.State, t.State) {
				err = errors.Join(err, fmt.Errorf("资产 %s 未恢复到 %s", t.Execution.Asset.Name, t.State))
			}
		}
		restored = &old
		_, serviceErr := w.serviceRules(ctx, op, p, old, p.BeforeBindings)
		err = errors.Join(err, serviceErr)
		if err == nil {
			_, err = w.batch(ctx, op, p, api.NodePlanPhaseCleanupVolumes, removedVolumes(p.Updates, before))
		}
	} else {
		err = errors.Join(err, w.network(ctx, op, p, true))
	}
	if errors.Is(err, errPersistence) || ctx.Err() != nil {
		return errors.Join(initial, err, ctx.Err())
	}
	if err != nil {
		message := errors.Join(initial, err).Error()
		return errors.Join(initial, err, w.Queries.SetEnvironmentState(ctx, queries.SetEnvironmentStateParams{ID: *op.EnvironmentID, Status: "failed", Error: &message}))
	}
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", errPersistence, err)
	}
	defer tx.Rollback(ctx)
	q := w.Queries.WithTx(tx)
	if _, err = q.LockEnvironment(ctx, *op.EnvironmentID); err != nil {
		return fmt.Errorf("%w: %v", errPersistence, err)
	}
	retained := api.EnvironmentSpec{}
	if p.BeforeSpec != nil {
		retained = *p.BeforeSpec
	}
	if err = releaseExternal(ctx, q, *op.EnvironmentID, retained); err != nil {
		return fmt.Errorf("%w: %v", errPersistence, err)
	}
	if err = q.DeleteUnusedVPNAliases(ctx, *op.EnvironmentID); err != nil {
		return fmt.Errorf("%w: %v", errPersistence, err)
	}
	if p.Gateway != nil {
		if err = commitServices(ctx, q, *op.EnvironmentID, p.BeforeBindings); err != nil {
			return fmt.Errorf("%w: %v", errPersistence, err)
		}
		if p.BeforeSpec == nil {
			if err = q.SetGatewayAddress(ctx, queries.SetGatewayAddressParams{EnvironmentID: *op.EnvironmentID}); err != nil {
				return fmt.Errorf("%w: %v", errPersistence, err)
			}
		}
	}
	raw, err := resourceRecords(*op.EnvironmentID, before)
	if err != nil {
		return err
	}
	if err = q.UpdateExecutions(ctx, raw); err != nil {
		return fmt.Errorf("%w: %v", errPersistence, err)
	}
	transactionWorker := w
	transactionWorker.Queries = q
	if restored != nil && !reflect.DeepEqual(*restored, *p.BeforeSpec) {
		raw, err = json.Marshal(restored)
		if err != nil {
			return err
		}
		state, stateErr := runtimeState(ctx, q, *op.EnvironmentID)
		if stateErr != nil {
			return fmt.Errorf("%w: %v", errPersistence, stateErr)
		}
		message := initial.Error()
		if err = q.CommitEnvironment(ctx, queries.CommitEnvironmentParams{ID: *op.EnvironmentID, AppliedSpec: raw, Status: state, Error: &message}); err != nil {
			return fmt.Errorf("%w: %v", errPersistence, err)
		}
		p.Committed = true
	} else if err = transactionWorker.status(ctx, op, p, initial); err != nil {
		return fmt.Errorf("%w: %v", errPersistence, err)
	}
	if err = transactionWorker.phase(ctx, op, p, "rolled-back"); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: %v", errPersistence, err)
	}
	return initial
}

func runtimeState(ctx context.Context, q *queries.Queries, id string) (string, error) {
	assets, err := q.ListRuntimeAssets(ctx, id)
	if err != nil {
		return "", err
	}
	states := map[string]bool{}
	for _, a := range assets {
		if !a.Current {
			continue
		}
		state := a.State
		if matches(state, "stopped") {
			state = "stopped"
		}
		states[state] = true
	}
	if len(states) == 0 {
		row, readErr := q.GetEnvironment(ctx, id)
		if readErr != nil {
			return "", readErr
		}
		if row.AppliedSpec == nil {
			return "draft", nil
		}
		return "stopped", nil
	}
	if states["unknown"] || states["absent"] || states["reserved"] {
		return "unknown", nil
	}
	for _, state := range []string{"running", "suspended", "stopped"} {
		if states[state] {
			return state, nil
		}
	}
	return "unknown", nil
}

func (w Worker) status(ctx context.Context, op *queries.Operation, p *Payload, failure error) error {
	state, err := runtimeState(ctx, w.Queries, *op.EnvironmentID)
	if err != nil {
		return err
	}
	var detail *string
	if failure != nil {
		message := failure.Error()
		detail = &message
		if state == "draft" && len(p.Targets) > 0 {
			state = "failed"
		}
	}
	return w.Queries.SetEnvironmentState(ctx, queries.SetEnvironmentStateParams{ID: *op.EnvironmentID, Status: state, Error: detail})
}
