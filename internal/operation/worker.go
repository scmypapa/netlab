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
	"netlab.local/core/internal/environment"
	"netlab.local/core/internal/secret"
	"netlab.local/core/internal/transport"
)

type Target struct {
	NodeID    string             `json:"nodeId"`
	Execution api.AssetExecution `json:"execution"`
	State     string             `json:"state"`
}
type Payload struct {
	RetirementChild     *string                    `json:"retirementChild,omitempty"`
	NetworkSource       string                     `json:"networkSource,omitempty"`
	Migration           *Migration                 `json:"migration,omitempty"`
	Volume              *api.NodeVolume            `json:"volume,omitempty"`
	VolumeNode          string                     `json:"volumeNode,omitempty"`
	BackupID            *string                    `json:"backupId,omitempty"`
	BackupInitialize    bool                       `json:"backupInitialize,omitempty"`
	BackupNativeID      *string                    `json:"backupNativeId,omitempty"`
	BackupResult        *api.NodeBackupResult      `json:"backupResult,omitempty"`
	Recovery            *Recovery                  `json:"recovery,omitempty"`
	Run                 bool                       `json:"run,omitempty"`
	StoragePool         *api.CreateStoragePool     `json:"storagePool,omitempty"`
	StorageDevice       *api.ConfigureNodeStorage  `json:"storageDevice,omitempty"`
	CephDevices         map[string]string          `json:"cephDevices,omitempty"`
	CephReplicas        *int                       `json:"cephReplicas,omitempty"`
	CephJoinNodes       []string                   `json:"cephJoinNodes,omitempty"`
	Spec                api.EnvironmentSpec        `json:"spec"`
	BeforeStatus        string                     `json:"beforeStatus,omitempty"`
	Template            *api.Template              `json:"template,omitempty"`
	TemplateCredentials []byte                     `json:"templateCredentials,omitempty"`
	TemplateCapture     *api.TemplateCaptureSource `json:"templateCapture,omitempty"`
	Targets             []Target                   `json:"targets,omitempty"`
	Old                 []Target                   `json:"old,omitempty"`
	Unchanged           []Target                   `json:"unchanged,omitempty"`
	Updates             []Target                   `json:"updates,omitempty"`
	Owner               *Target                    `json:"owner,omitempty"`
	ExternalChassis     map[string]string          `json:"externalChassis,omitempty"`
	Committed           bool                       `json:"committed,omitempty"`
	BeforeSpec          *api.EnvironmentSpec       `json:"beforeSpec,omitempty"`
	Before              []Target                   `json:"before,omitempty"`
	Results             []api.ExecutionResult      `json:"results,omitempty"`
	Failure             *string                    `json:"failure,omitempty"`
	Gateway             *api.ServiceGateway        `json:"gateway,omitempty"`
	Bindings            []api.NodeServiceBinding   `json:"bindings,omitempty"`
	BeforeBindings      []api.NodeServiceBinding   `json:"beforeBindings,omitempty"`
	VPNChange           *environment.VPNChange     `json:"vpnChange,omitempty"`
	VPNPlan             *api.NodeVPNPlan           `json:"vpnPlan,omitempty"`
	VPNBefore           *api.NodeVPNPlan           `json:"vpnBefore,omitempty"`
	VPNResult           *api.NodeVPNResult         `json:"vpnResult,omitempty"`
}
type Worker struct {
	Pool    *pgxpool.Pool
	Queries *queries.Queries
	Client  *transport.Client
	Secrets *secret.Cipher
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
		if op.Kind == "prepare-template" || op.Kind == "capture-template" {
			err = w.prepareTemplate(ctx, &op, &payload)
		} else if op.Kind == "trim-template-cache" {
			err = w.trimTemplateCache(ctx, &op, &payload)
		} else if op.Kind == "delete-template" {
			err = w.deleteTemplate(ctx, &op, &payload)
		} else if op.Kind == "delete-storage-pool" {
			err = w.deleteStoragePool(ctx, &op, &payload)
		} else if op.Kind == "configure-storage-pool" {
			err = w.configureStoragePool(ctx, &op, &payload)
		} else if op.Kind == "configure-node-storage" {
			err = w.configureNodeStorage(ctx, &op, &payload)
		} else if op.ScopeKind == "volume" {
			err = w.volume(ctx, &op, &payload)
		} else if op.ScopeKind == "backup" || op.ScopeKind == "backup-repository" {
			err = w.backup(ctx, &op, &payload)
		} else if op.Kind == "capture-recovery" || op.Kind == "delete-recovery" {
			err = w.recovery(ctx, &op, &payload)
			results = payload.Results
		} else if op.Kind == "vpn-create" || op.Kind == "vpn-revoke" {
			err = w.vpnOperation(ctx, &op, &payload)
		} else if op.Kind == "migrate" {
			err = w.migration(ctx, &op, &payload)
			results = payload.Results
		} else if op.Kind == "retire-node" {
			err = w.retireNode(ctx, &op, &payload)
		} else if op.Kind == "move-network" {
			err = w.moveNetwork(ctx, &op, &payload)
		} else {
			err = w.environment(ctx, &op, &payload)
			results = payload.Results
		}
	}
	if ctx.Err() != nil || errors.Is(err, errPersistence) {
		return
	}
	if err != nil && payload.Template != nil && op.Kind != "trim-template-cache" {
		failed, message := api.TemplateStateFailed, err.Error()
		if op.Kind == "delete-template" {
			failed = api.TemplateStateDeleting
		}
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
	if op.ScopeKind == "volume" {
		if op.Kind == "delete-volume" && err == nil {
			dbErr = q.DeletePersistentVolume(ctx, op.ScopeID)
		} else {
			volumeState := "ready"
			size := payload.Volume.SizeGiB
			if err != nil {
				volumeState = "failed"
				row, readErr := q.GetPersistentVolume(ctx, op.ScopeID)
				if readErr != nil {
					slog.Error("volume completion", "error", readErr)
					return
				}
				size = row.SizeGib
			}
			dbErr = q.FinishVolume(ctx, queries.FinishVolumeParams{ID: op.ScopeID, State: volumeState, SizeGib: size})
		}
		if dbErr != nil {
			slog.Error("volume completion", "error", dbErr)
			return
		}
	}
	if op.ScopeKind == "backup" || op.ScopeKind == "backup-repository" {
		if dbErr = w.finishBackup(ctx, q, op, &payload, err); dbErr != nil {
			slog.Error("backup completion", "error", dbErr)
			return
		}
	}
	if op.Kind == "capture-recovery" || op.Kind == "delete-recovery" {
		if dbErr = w.finishRecovery(ctx, q, op, &payload, err); dbErr != nil {
			slog.Error("recovery completion", "error", dbErr)
			return
		}
		actual := payload.BeforeStatus
		var runtimeError *string
		if op.Kind == "capture-recovery" {
			actual, dbErr = runtimeState(ctx, q, *op.EnvironmentID)
			if dbErr != nil {
				slog.Error("recovery runtime state", "error", dbErr)
				return
			}
			if err != nil && op.Phase == "recovery-resume" {
				actual, runtimeError = "failed", detail
			}
		}
		if dbErr = q.FinishAuxiliaryOperation(ctx, queries.FinishAuxiliaryOperationParams{ID: *op.EnvironmentID, OperationID: &op.ID, Status: actual, Error: runtimeError}); dbErr != nil {
			slog.Error("recovery environment completion", "error", dbErr)
			return
		}
	}
	if op.Kind == "delete-storage-pool" && err == nil {
		if dbErr = q.DeleteStoragePool(ctx, op.ScopeID); dbErr != nil {
			slog.Error("storage deletion completion", "error", dbErr)
			return
		}
	}
	if op.Kind == "delete-template" && err == nil {
		if dbErr = q.DeleteTemplate(ctx, payload.Template.Id); dbErr != nil {
			slog.Error("template deletion completion", "error", dbErr)
			return
		}
	}
	if op.Kind == "capture-template" {
		if dbErr = q.FinishAuxiliaryOperation(ctx, queries.FinishAuxiliaryOperationParams{ID: *op.EnvironmentID, OperationID: &op.ID, Status: payload.BeforeStatus}); dbErr != nil {
			slog.Error("template capture completion", "error", dbErr)
			return
		}
	}
	if op.EnvironmentID != nil {
		if dbErr = q.DeleteUnusedGuestConnections(ctx, *op.EnvironmentID); dbErr != nil {
			slog.Error("guest connection cleanup", "error", dbErr)
			return
		}
	}
	count, dbErr := q.FinishOperation(ctx, queries.FinishOperationParams{ID: op.ID, LeaseOwner: op.LeaseOwner, State: state, Phase: phase, Results: raw, Error: detail})
	if dbErr != nil || count != 1 {
		slog.Error("operation completion", "error", dbErr)
		return
	}
	if op.Kind == "retire-node" && err == nil {
		if dbErr = q.DeleteRetiredNode(ctx, op.ScopeID); dbErr != nil {
			slog.Error("node retirement completion", "error", dbErr)
			return
		}
	}
	if _, dbErr = q.AddEvent(ctx, queries.AddEventParams{EnvironmentID: op.EnvironmentID, Kind: "operation." + state, Payload: raw}); dbErr != nil {
		slog.Error("operation event", "error", dbErr)
		return
	}
	if dbErr = tx.Commit(ctx); dbErr != nil {
		slog.Error("operation commit", "error", dbErr)
		return
	}
	if (op.Kind == "configure-storage-pool" || op.Kind == "configure-node-storage") && err == nil {
		if dbErr = (Service{Pool: w.Pool, Queries: w.Queries}).ConfigureManagedStorage(ctx, nil); dbErr != nil {
			slog.Error("storage membership", "error", dbErr)
		}
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
	n, err := TemplateNode(nodes, t)
	if err != nil {
		return err
	}
	t.ArtifactNodeId = &n.ID
	p.Template = &t
	if err = w.phase(ctx, op, p, "prepare"); err != nil {
		return err
	}
	var prepared api.Template
	request := api.NodeTemplatePreparation{Template: t, Capture: p.TemplateCapture}
	if len(p.TemplateCredentials) > 0 {
		raw, err := w.Secrets.Decrypt(p.TemplateCredentials, fmt.Sprintf("%s/%d", t.Id, t.Version))
		if err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &request.Registry); err != nil {
			return err
		}
	}
	err = w.Client.Do(ctx, "POST", n.Endpoint, "/node/v1/templates/prepare", request, &prepared)
	if err != nil {
		failed := api.TemplateStateFailed
		t.State = &failed
		message := err.Error()
		t.Error = &message
	} else {
		t = prepared
	}
	p.Template = &t
	raw, marshalErr := json.Marshal(t)
	if marshalErr != nil {
		return marshalErr
	}
	if saveErr := w.Queries.UpdateTemplate(ctx, queries.UpdateTemplateParams{ID: t.Id, Definition: raw}); saveErr != nil {
		return fmt.Errorf("%w: %v", errPersistence, saveErr)
	}
	return err
}
func (w Worker) batch(ctx context.Context, op *queries.Operation, p *Payload, phase api.NodePlanPhase, targets []Target) ([]api.ExecutionResult, error) {
	if len(targets) == 0 {
		return []api.ExecutionResult{}, nil
	}
	artifacts := map[string]string{}
	if phase == api.NodePlanPhasePrepare || phase == api.NodePlanPhasePrepareRecovery {
		for _, target := range targets {
			if origin := target.Execution.Template.ArtifactNodeId; origin != nil {
				artifacts[*origin] = ""
			}
		}
		ids := make([]string, 0, len(artifacts))
		for id := range artifacts {
			ids = append(ids, id)
		}
		origins, err := w.Queries.GetNodeEndpoints(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errPersistence, err)
		}
		if len(origins) != len(ids) {
			return nil, errors.New("模板制品节点不存在")
		}
		for _, origin := range origins {
			artifacts[origin.ID] = origin.Endpoint
		}
		if phase == api.NodePlanPhasePrepare {
			if err = w.prepareSharedTemplates(ctx, targets, artifacts); err != nil {
				return nil, err
			}
		}
	}
	grouped := map[string][]api.AssetExecution{}
	captureStates := map[string]string{}
	if p.Recovery != nil {
		for _, target := range p.Recovery.Assets {
			captureStates[target.Execution.Asset.Id] = target.State
		}
	}
	recoverySources := map[string]api.NodeRecoverySource{}
	if phase == api.NodePlanPhasePrepareRecovery {
		var err error
		recoverySources, err = w.recoverySources(ctx, p)
		if err != nil {
			return nil, err
		}
	}
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
			plan := api.NodePlan{OperationId: op.ID, EnvironmentId: *op.EnvironmentID, Phase: phase, Assets: assets, Spec: spec, ArtifactEndpoints: &artifacts}
			if phase == api.NodePlanPhaseRollbackRecovery || phase == api.NodePlanPhaseCleanupRecovery {
				plan.RecoveryTargets = map[string]api.AssetExecution{}
				for _, target := range desiredTargets(p) {
					plan.RecoveryTargets[target.Execution.Asset.Id] = target.Execution
				}
			}
			if p.Recovery != nil {
				plan.RecoveryPointId = &p.Recovery.ID
				plan.IncludeMemory = p.Recovery.IncludeMemory
				plan.CaptureStates = &captureStates
				plan.RecoverySources = &recoverySources
			}
			result, err := w.Client.Execute(ctx, endpoints[id], plan)
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
				valid[len(valid)-1].NodeId = &id
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
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return results, errors.Join(executionErr, fmt.Errorf("%w: %v", errPersistence, err))
	}
	defer tx.Rollback(ctx)
	q := w.Queries.WithTx(tx)
	if phase != api.NodePlanPhaseCleanupVolumes && phase != api.NodePlanPhaseCaptureRecovery && phase != api.NodePlanPhaseDeleteRecovery && phase != api.NodePlanPhasePrepareRecovery && phase != api.NodePlanPhaseCleanupRecovery {
		if err = q.ApplyAssetResults(ctx, raw); err != nil {
			return results, errors.Join(executionErr, fmt.Errorf("%w: %v", errPersistence, err))
		}
	}
	transactionWorker := w
	transactionWorker.Queries = q
	if err = transactionWorker.phase(ctx, op, p, op.Phase); err != nil {
		return results, errors.Join(executionErr, err)
	}
	if err = tx.Commit(ctx); err != nil {
		return results, errors.Join(executionErr, fmt.Errorf("%w: %v", errPersistence, err))
	}
	return results, executionErr
}
func (w Worker) network(ctx context.Context, op *queries.Operation, p *Payload, remove bool) error {
	if remove {
		if err := w.removeCaptures(ctx, *op.EnvironmentID); err != nil {
			return err
		}
	}
	if !remove {
		specs := []api.EnvironmentSpec{p.Spec}
		if p.BeforeSpec != nil && op.Phase != "rollback" {
			specs = append(specs, *p.BeforeSpec)
		}
		if err := w.external(ctx, op, specs...); err != nil {
			return err
		}
	}
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
	result, err := w.Client.Execute(ctx, nodes[0].Endpoint, api.NodePlan{OperationId: op.ID, EnvironmentId: *op.EnvironmentID, Phase: phase, Assets: assets, Spec: p.Spec, Gateway: p.Gateway, Services: &p.Bindings, ExternalChassis: &p.ExternalChassis})
	if err != nil {
		return err
	}
	if result.Error != nil {
		return errors.New(*result.Error)
	}
	if remove {
		return w.external(ctx, op)
	}
	return nil
}
