package operation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
)

// Recovery stores the applied configuration and the actual asset identities at capture time.
type Recovery struct {
	EnvironmentID string                         `json:"environmentId"`
	ID            string                         `json:"id"`
	Spec          api.EnvironmentSpec            `json:"spec"`
	Assets        []Target                       `json:"assets"`
	Bytes         int64                          `json:"bytes"`
	IncludeMemory bool                           `json:"includeMemory,omitempty"`
	Captures      map[string]api.RecoveryCapture `json:"captures,omitempty"`
	Consistency   api.RecoveryConsistency        `json:"consistency,omitempty"`
}

func restoresData(kind string) bool { return kind == "restore-recovery" || kind == "clone-recovery" }

func (w Worker) recovery(ctx context.Context, op *queries.Operation, p *Payload) error {
	if op.Kind == "delete-recovery" {
		if err := w.phase(ctx, op, p, "delete-recovery"); err != nil {
			return err
		}
		_, err := w.batch(ctx, op, p, api.NodePlanPhaseDeleteRecovery, p.Recovery.Assets)
		return err
	}
	if op.Phase == "recovery-reset" {
		if _, err := w.batch(ctx, op, p, api.NodePlanPhaseDeleteRecovery, p.Recovery.Assets); err != nil {
			return err
		}
		if err := w.phase(ctx, op, p, "queued"); err != nil {
			return err
		}
	}
	if op.Phase == "queued" {
		observed, err := w.batch(ctx, op, p, api.NodePlanPhaseInspect, p.Recovery.Assets)
		if err != nil {
			return err
		}
		for i := range p.Recovery.Assets {
			t := &p.Recovery.Assets[i]
			for _, r := range observed {
				if r.InstanceId == t.Execution.InstanceId {
					t.State = r.State
					if r.Execution != nil {
						t.Execution = *r.Execution
					}
				}
			}
			if t.State != "running" && t.State != "suspended" && !matches(t.State, "stopped") {
				return fmt.Errorf("资产 %s 当前状态 %s 无法捕获", t.Execution.Asset.Name, t.State)
			}
		}
		if err = w.phase(ctx, op, p, "recovery-quiesce"); err != nil {
			return err
		}
	}
	for op.Phase != "recovery-complete" {
		var err error
		next := ""
		switch op.Phase {
		case "recovery-quiesce":
			active := slices.DeleteFunc(slices.Clone(p.Recovery.Assets), func(t Target) bool { return t.State != "running" || t.Execution.Template.Kind == api.Vm })
			_, err = w.batch(ctx, op, p, api.NodePlanPhaseSuspend, active)
			next = "recovery-capture"
		case "recovery-capture":
			var results []api.ExecutionResult
			results, err = w.batch(ctx, op, p, api.NodePlanPhaseCaptureRecovery, p.Recovery.Assets)
			p.Recovery.Bytes = 0
			p.Recovery.Captures = make(map[string]api.RecoveryCapture, len(results))
			p.Recovery.Consistency = api.Application
			if len(results) == 0 {
				p.Recovery.Consistency = api.Crash
			}
			for _, result := range results {
				if result.Error == nil {
					if result.Recovery == nil {
						err = errors.Join(err, fmt.Errorf("资产 %s 未返回捕获数据", result.AssetId))
					} else {
						p.Recovery.Bytes += result.Recovery.SizeBytes
						p.Recovery.Captures[result.AssetId] = *result.Recovery
						if result.Recovery.Consistency == api.Crash || (result.Recovery.Consistency == api.Filesystem && p.Recovery.Consistency == api.Application) {
							p.Recovery.Consistency = result.Recovery.Consistency
						}
					}
				}
			}
			next = "recovery-resume"
		case "recovery-resume":
			err = w.activate(ctx, op, p, p.Recovery.Assets)
			if err == nil {
				var results []api.ExecutionResult
				results, err = w.batch(ctx, op, p, api.NodePlanPhaseInspect, p.Recovery.Assets)
				for _, result := range results {
					target := findInstance(p.Recovery.Assets, result.InstanceId)
					if !matches(result.State, target.State) && !(matches(target.State, "stopped") && matches(result.State, "stopped")) {
						err = errors.Join(err, fmt.Errorf("资产 %s 未恢复到 %s", target.Execution.Asset.Name, target.State))
					}
				}
			}
			if err != nil {
				return fmt.Errorf("recovery-resume: %w", err)
			}
			next = "recovery-complete"
		default:
			return fmt.Errorf("invalid recovery phase %s", op.Phase)
		}
		if ctx.Err() != nil || errors.Is(err, errPersistence) {
			return err
		}
		if err != nil {
			message := fmt.Sprintf("%s: %v", op.Phase, err)
			p.Failure = &message
			next = "recovery-resume"
		}
		if err = w.phase(ctx, op, p, next); err != nil {
			return err
		}
	}
	if p.Failure != nil {
		return errors.New(*p.Failure)
	}
	return nil
}

func (w Worker) finishRecovery(ctx context.Context, q *queries.Queries, op queries.Operation, p *Payload, failure error) error {
	if op.Kind == "delete-recovery" {
		if failure == nil {
			return q.DeleteRecoveryPoint(ctx, p.Recovery.ID)
		}
		return nil
	}
	state := "ready"
	if failure != nil {
		state = "failed"
	}
	raw, err := json.Marshal(p.Recovery)
	if err != nil {
		return err
	}
	return q.CompleteRecoveryPoint(ctx, queries.CompleteRecoveryPointParams{ID: p.Recovery.ID, State: state, Definition: raw, SizeBytes: p.Recovery.Bytes})
}
