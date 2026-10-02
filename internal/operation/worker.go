package operation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/transport"
)

type Target struct {
	NodeID    string             `json:"nodeId"`
	Execution api.AssetExecution `json:"execution"`
	State     string             `json:"state"`
}
type Payload struct {
	Spec         api.EnvironmentSpec   `json:"spec"`
	BeforeStatus string                `json:"beforeStatus,omitempty"`
	Template     *api.Template         `json:"template,omitempty"`
	Targets      []Target              `json:"targets,omitempty"`
	Old          []Target              `json:"old,omitempty"`
	Unchanged    []Target              `json:"unchanged,omitempty"`
	Updates      []Target              `json:"updates,omitempty"`
	Owner        *Target               `json:"owner,omitempty"`
	Committed    bool                  `json:"committed,omitempty"`
	BeforeSpec   *api.EnvironmentSpec  `json:"beforeSpec,omitempty"`
	Before       []Target              `json:"before,omitempty"`
	Results      []api.ExecutionResult `json:"results,omitempty"`
	Failure      *string               `json:"failure,omitempty"`
}
type Worker struct {
	Pool    *pgxpool.Pool
	Queries *queries.Queries
	Client  *transport.Client
}

var errPersistence = errors.New("持久状态提交结果尚未确认")

func (w Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			owner := uuid.NewString()
			timer := time.NewTicker(time.Second)
			defer timer.Stop()
			for ctx.Err() == nil {
				op, err := w.Queries.ClaimOperation(ctx, &owner)
				var pgerr *pgconn.PgError
				if errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &pgerr) && pgerr.ConstraintName == "operations_one_running") {
					select {
					case <-ctx.Done():
						return
					case <-timer.C:
						continue
					}
				}
				if err != nil {
					slog.Error("operation claim", "error", err)
					select {
					case <-ctx.Done():
						return
					case <-timer.C:
						continue
					}
				}
				w.execute(ctx, queries.Operation(op))
			}
		}()
	}
	wg.Wait()
}
func (w Worker) execute(parent context.Context, op queries.Operation) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-finished:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				count, err := w.Queries.RenewLease(ctx, queries.RenewLeaseParams{ID: op.ID, LeaseOwner: op.LeaseOwner})
				if err != nil || count != 1 {
					cancel()
					return
				}
			}
		}
	}()
	var payload Payload
	err := json.Unmarshal(op.Payload, &payload)
	results := []api.ExecutionResult{}
	if err == nil {
		if op.Kind == "prepare-template" {
			err = w.prepareTemplate(ctx, &op, &payload)
		} else {
			err = w.environment(ctx, &op, &payload)
			results = payload.Results
		}
	}
	if ctx.Err() != nil || errors.Is(err, errPersistence) {
		return
	}
	if err != nil && payload.Template != nil {
		failed, message := api.Failed, err.Error()
		payload.Template.State = &failed
		payload.Template.Error = &message
		raw, saveErr := json.Marshal(payload.Template)
		if saveErr == nil {
			saveErr = w.Queries.UpdateTemplate(ctx, queries.UpdateTemplateParams{ID: payload.Template.Id, Definition: raw})
		}
		if saveErr != nil {
			slog.Error("template completion", "error", saveErr)
			return
		}
	}
	state, phase := "succeeded", "complete"
	var detail *string
	if err != nil {
		state, phase = "failed", op.Phase
		if payload.Committed {
			state = "partially_applied"
		}
		s := err.Error()
		detail = &s
		slog.Error("operation failed", "operation", op.ID, "phase", op.Phase, "error", err)
	}
	raw, marshalErr := json.Marshal(results)
	if marshalErr != nil {
		slog.Error("operation result", "error", marshalErr)
		return
	}
	tx, dbErr := w.Pool.Begin(ctx)
	if dbErr != nil {
		slog.Error("operation completion", "error", dbErr)
		return
	}
	defer tx.Rollback(ctx)
	q := w.Queries.WithTx(tx)
	count, dbErr := q.FinishOperation(ctx, queries.FinishOperationParams{ID: op.ID, LeaseOwner: op.LeaseOwner, State: state, Phase: phase, Results: raw, Error: detail})
	if dbErr != nil || count != 1 {
		slog.Error("operation completion", "error", dbErr)
		return
	}
	if _, dbErr = q.AddEvent(ctx, queries.AddEventParams{EnvironmentID: op.EnvironmentID, Kind: "operation." + state, Payload: raw}); dbErr != nil {
		slog.Error("operation event", "error", dbErr)
		return
	}
	if dbErr = tx.Commit(ctx); dbErr != nil {
		slog.Error("operation commit", "error", dbErr)
	}
}
func (w Worker) phase(ctx context.Context, op *queries.Operation, p *Payload, phase string) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	results, err := json.Marshal(p.Results)
	if err != nil {
		return err
	}
	n, err := w.Queries.SaveOperationProgress(ctx, queries.SaveOperationProgressParams{ID: op.ID, LeaseOwner: op.LeaseOwner, Phase: phase, Payload: raw, Results: results})
	if err != nil {
		return fmt.Errorf("%w: %v", errPersistence, err)
	}
	if n != 1 {
		return fmt.Errorf("%w: operation ownership changed", errPersistence)
	}
	op.Phase = phase
	return nil
}
func (w Worker) prepareTemplate(ctx context.Context, op *queries.Operation, p *Payload) error {
	nodes, err := w.Queries.ListNodes(ctx)
	if err != nil {
		return err
	}
	if p.Template == nil {
		return errors.New("missing template definition")
	}
	t := *p.Template
	for _, n := range nodes {
		var info api.NodeInfo
		if err = json.Unmarshal(n.Info, &info); err != nil {
			return err
		}
		if n.State != "ready" || !slices.Contains(info.Capabilities, string(t.Kind)) {
			continue
		}
		if err = w.phase(ctx, op, p, "prepare"); err != nil {
			return err
		}
		var prepared api.Template
		err = w.Client.Do(ctx, "POST", n.Endpoint, "/node/v1/templates/prepare", t, &prepared)
		if err != nil {
			failed := api.Failed
			t.State = &failed
			message := err.Error()
			t.Error = &message
		} else {
			t = prepared
		}
		raw, marshalErr := json.Marshal(t)
		if marshalErr != nil {
			return marshalErr
		}
		if saveErr := w.Queries.UpdateTemplate(ctx, queries.UpdateTemplateParams{ID: t.Id, Definition: raw}); saveErr != nil {
			return saveErr
		}
		return err
	}
	return errors.New("没有可准备该模板的节点")
}
func (w Worker) batch(ctx context.Context, op *queries.Operation, p *Payload, phase api.NodePlanPhase, targets []Target) ([]api.ExecutionResult, error) {
	if len(targets) == 0 {
		return []api.ExecutionResult{}, nil
	}
	grouped := map[string][]api.AssetExecution{}
	endpoints := map[string]string{}
	for _, t := range targets {
		grouped[t.NodeID] = append(grouped[t.NodeID], t.Execution)
	}
	ids := make([]string, 0, len(grouped))
	for id := range grouped {
		ids = append(ids, id)
	}
	nodes, err := w.Queries.GetNodeEndpoints(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errPersistence, err)
	}
	for _, n := range nodes {
		endpoints[n.ID] = n.Endpoint
	}
	if len(nodes) != len(grouped) {
		return nil, errors.New("运行资产所属节点不存在")
	}
	type response struct {
		results []api.ExecutionResult
		err     error
	}
	responses := make(chan response, len(grouped))
	var wg sync.WaitGroup
	for id, assets := range grouped {
		wg.Add(1)
		go func(id string, assets []api.AssetExecution) {
			defer wg.Done()
			spec := p.Spec
			if op.Phase == "rollback" && p.BeforeSpec != nil {
				spec = *p.BeforeSpec
			}
			result, err := w.Client.Execute(ctx, endpoints[id], api.NodePlan{OperationId: op.ID, EnvironmentId: *op.EnvironmentID, Phase: phase, Assets: assets, Spec: spec})
			if err == nil && result.Error != nil {
				err = errors.New(*result.Error)
			}
			expected := map[string]string{}
			for _, a := range assets {
				expected[a.InstanceId] = a.Asset.Id
			}
			observed := map[string]api.ExecutionResult{}
			valid := make([]api.ExecutionResult, 0, len(result.Results))
			for _, r := range result.Results {
				if expected[r.InstanceId] != r.AssetId || expected[r.InstanceId] == "" {
					err = errors.Join(err, fmt.Errorf("节点返回了请求范围外的资产结果 %s", r.InstanceId))
					continue
				}
				valid = append(valid, r)
				observed[r.InstanceId] = r
				if r.Error != nil {
					err = errors.Join(err, fmt.Errorf("asset %s: %s", r.AssetId, *r.Error))
				}
			}
			for _, a := range assets {
				r, ok := observed[a.InstanceId]
				if !ok || r.AssetId != a.Asset.Id {
					err = errors.Join(err, fmt.Errorf("node omitted asset result %s", a.Asset.Name))
				}
			}
			responses <- response{valid, err}
		}(id, assets)
	}
	wg.Wait()
	close(responses)
	results := []api.ExecutionResult{}
	var executionErr error
	for r := range responses {
		results = append(results, r.results...)
		executionErr = errors.Join(executionErr, r.err)
	}
	latest := map[string]api.ExecutionResult{}
	for _, r := range p.Results {
		latest[r.InstanceId] = r
	}
	for _, r := range results {
		latest[r.InstanceId] = r
	}
	p.Results = make([]api.ExecutionResult, 0, len(latest))
	for _, r := range latest {
		p.Results = append(p.Results, r)
	}
	slices.SortFunc(p.Results, func(a, b api.ExecutionResult) int { return strings.Compare(a.InstanceId, b.InstanceId) })
	raw, err := json.Marshal(results)
	if err != nil {
		return results, err
	}
	if phase != api.NodePlanPhaseCleanupVolumes {
		if err = w.Queries.ApplyAssetResults(ctx, raw); err != nil {
			return results, errors.Join(executionErr, fmt.Errorf("%w: %v", errPersistence, err))
		}
	}
	if err = w.phase(ctx, op, p, op.Phase); err != nil {
		return results, errors.Join(executionErr, err)
	}
	return results, executionErr
}
func (w Worker) network(ctx context.Context, op *queries.Operation, p *Payload, remove bool) error {
	if p.Owner == nil {
		if len(p.Spec.Networks) == 0 {
			return nil
		}
		return errors.New("missing network owner")
	}
	phase := api.NodePlanPhaseNetwork
	if remove {
		phase = api.NodePlanPhaseRemoveNetwork
	}
	assets := make([]api.AssetExecution, 0, len(p.Targets)+len(p.Unchanged)+len(p.Updates))
	for _, group := range [][]Target{p.Targets, p.Unchanged, p.Updates} {
		for _, t := range group {
			assets = append(assets, t.Execution)
		}
	}
	nodes, err := w.Queries.GetNodeEndpoints(ctx, []string{p.Owner.NodeID})
	if err != nil {
		return fmt.Errorf("%w: %v", errPersistence, err)
	}
	if len(nodes) == 0 {
		return errors.New("环境网络所属节点不存在")
	}
	result, err := w.Client.Execute(ctx, nodes[0].Endpoint, api.NodePlan{OperationId: op.ID, EnvironmentId: *op.EnvironmentID, Phase: phase, Assets: assets, Spec: p.Spec})
	if err != nil {
		return err
	}
	if result.Error != nil {
		return errors.New(*result.Error)
	}
	return nil
}
