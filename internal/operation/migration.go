package operation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/environment"
)

type Migration struct {
	RequestedNode *string `json:"requestedNode,omitempty"`
	Target        *Target `json:"target,omitempty"`
	DomainXML     string  `json:"domainXml,omitempty"`
	Live          bool    `json:"live"`
}

func (s Service) Migrate(ctx context.Context, identity access.Identity, id, asset string, input api.MigrationRequest) (api.Operation, error) {
	service := environment.Service{Pool: s.Pool, Queries: s.Queries}
	if _, err := service.Authorized(ctx, identity, id, "manage", asset); err != nil {
		return api.Operation{}, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return api.Operation{}, err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	row, err := q.LockEnvironment(ctx, id)
	if err != nil {
		return api.Operation{}, err
	}
	if input.ClientRequestId != nil {
		previous, readErr := q.GetOperationByRequest(ctx, queries.GetOperationByRequestParams{EnvironmentID: &id, ClientRequestID: input.ClientRequestId})
		if readErr == nil {
			if previous.Kind != "migrate" || previous.AssetID == nil || *previous.AssetID != asset {
				return api.Operation{}, environment.ErrConflict
			}
			return environment.Operation(previous)
		}
		if !errors.Is(readErr, pgx.ErrNoRows) {
			return api.Operation{}, readErr
		}
	}
	if int(row.Revision) != input.ExpectedRevision {
		return api.Operation{}, environment.ErrConflict
	}
	pending, err := q.PendingEnvironmentOperations(ctx, id)
	if err != nil {
		return api.Operation{}, err
	}
	if pending {
		return api.Operation{}, environment.ErrInUse
	}
	source, err := q.GetCurrentAsset(ctx, queries.GetCurrentAssetParams{EnvironmentID: id, AssetID: asset})
	if err != nil {
		return api.Operation{}, err
	}
	var execution api.AssetExecution
	if err = json.Unmarshal(source.Execution, &execution); err != nil {
		return api.Operation{}, err
	}
	if execution.Template.Kind != api.Vm && execution.Template.Kind != api.Container {
		return api.Operation{}, environment.Invalid("该资产不支持节点迁移")
	}
	if execution.Asset.PciBinding != nil {
		return api.Operation{}, environment.Invalid("直通设备属于当前宿主节点，解除绑定后可迁移")
	}
	if source.State != "running" && source.State != "suspended" && !matches(source.State, "stopped") {
		return api.Operation{}, environment.Invalid("当前资产状态无法迁移")
	}
	var spec api.EnvironmentSpec
	if err = json.Unmarshal(row.AppliedSpec, &spec); err != nil {
		return api.Operation{}, err
	}
	p := Payload{Spec: spec, BeforeStatus: row.Status, Migration: &Migration{RequestedNode: input.TargetNodeId}}
	raw, err := json.Marshal(p)
	if err != nil {
		return api.Operation{}, err
	}
	op, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: uuid.NewString(), EnvironmentID: &id, ScopeKind: "environment", ScopeID: id, Kind: "migrate", AssetID: &asset, Payload: raw, ExpectedRevision: row.Revision, ClientRequestID: input.ClientRequestId})
	if err != nil {
		return api.Operation{}, err
	}
	if err = q.SetEnvironmentOperation(ctx, queries.SetEnvironmentOperationParams{ID: id, OperationID: &op.ID, Status: "changing"}); err != nil {
		return api.Operation{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return api.Operation{}, err
	}
	return environment.Operation(op)
}

func migrationCandidates(source Target, nodes []queries.ListNodesRow, pools []queries.ListStoragePoolsRow, reserved []queries.GetNodeReservationsRow) ([]api.MigrationDestination, error) {
	if source.Execution.Asset.PciBinding != nil {
		return []api.MigrationDestination{}, nil
	}
	used := map[string]api.Resources{}
	for _, r := range reserved {
		used[r.NodeID] = api.Resources{Cpu: int(r.Cpu), MemoryMiB: r.MemoryMib}
	}
	result := []api.MigrationDestination{}
	for _, node := range nodes {
		if node.ID == source.NodeID || node.State != "ready" {
			continue
		}
		var info api.NodeInfo
		if err := json.Unmarshal(node.Info, &info); err != nil {
			return nil, err
		}
		if !supports(info, source.Execution.Template, source.Execution.Asset.Resources.Cpu) || len(source.Execution.Interfaces) > 0 && !slices.Contains(info.Capabilities, "network") {
			continue
		}
		capacity := info.Capacity
		if len(node.CapacityOverride) > 0 {
			if err := json.Unmarshal(node.CapacityOverride, &capacity); err != nil {
				return nil, err
			}
		}
		if !fits(add(used[node.ID], resources(source.Execution.Asset)), capacity) {
			continue
		}
		accessible := func(id string) bool {
			for _, pool := range pools {
				if pool.ID == id {
					return pool.State == "ready" && pool.Driver == "rbd" && slices.Contains(pool.NodeIds, node.ID)
				}
			}
			return false
		}
		live := source.Execution.Template.Kind == api.Vm && source.Execution.Rbd != nil && !matches(source.State, "stopped") && accessible(actualPool(source.NodeID, source.Execution))
		for _, volume := range source.Execution.VolumeSources {
			if volume.Storage.Rbd == nil || !accessible(volume.Storage.Rbd.SecretId) {
				live = false
				break
			}
		}
		result = append(result, api.MigrationDestination{Id: node.ID, Name: node.Name, Live: live, Available: api.Resources{Cpu: capacity.Cpu - used[node.ID].Cpu, MemoryMiB: capacity.MemoryMiB - used[node.ID].MemoryMiB}})
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Available.MemoryMiB == result[j].Available.MemoryMiB {
			return result[i].Id < result[j].Id
		}
		return result[i].Available.MemoryMiB > result[j].Available.MemoryMiB
	})
	return result, nil
}

func (s Service) MigrationDestinations(ctx context.Context, identity access.Identity, id, asset string) ([]api.MigrationDestination, error) {
	if _, err := (environment.Service{Queries: s.Queries}).Authorized(ctx, identity, id, "manage", asset); err != nil {
		return nil, err
	}
	row, err := s.Queries.GetCurrentAsset(ctx, queries.GetCurrentAssetParams{EnvironmentID: id, AssetID: asset})
	if err != nil {
		return nil, err
	}
	source := Target{NodeID: row.NodeID}
	if err = json.Unmarshal(row.Execution, &source.Execution); err != nil {
		return nil, err
	}
	source.State = row.State
	nodes, err := s.Queries.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, node := range nodes {
		ids = append(ids, node.ID)
	}
	reserved, err := s.Queries.GetNodeReservations(ctx, ids)
	if err != nil {
		return nil, err
	}
	pools, err := s.Queries.ListStoragePools(ctx)
	if err != nil {
		return nil, err
	}
	return migrationCandidates(source, nodes, pools, reserved)
}

func (w Worker) planMigration(ctx context.Context, op *queries.Operation, p *Payload) error {
	row, err := w.Queries.GetEnvironment(ctx, *op.EnvironmentID)
	if err != nil {
		return err
	}
	actual, err := w.Queries.ListRuntimeAssets(ctx, row.ID)
	if err != nil {
		return err
	}
	var source Target
	for _, a := range actual {
		if !a.Current {
			return environment.ErrInUse
		}
		t := Target{NodeID: a.NodeID, State: a.State}
		if err = json.Unmarshal(a.Execution, &t.Execution); err != nil {
			return err
		}
		p.Before = append(p.Before, t)
		if a.AssetID == *op.AssetID {
			source = t
		} else {
			p.Unchanged = append(p.Unchanged, t)
		}
	}
	if source.Execution.InstanceId == "" {
		return pgx.ErrNoRows
	}
	if source.State != "running" && source.State != "suspended" && !matches(source.State, "stopped") {
		return environment.Invalid("当前资产状态无法迁移")
	}
	if row.NetworkNodeID != nil {
		p.Owner = &Target{NodeID: *row.NetworkNodeID}
	}
	p.BeforeSpec = &p.Spec
	if err = w.loadServices(ctx, row, p); err != nil {
		return err
	}
	p.Bindings = slices.Clone(p.BeforeBindings)
	nodes, err := w.Queries.ListNodes(ctx)
	if err != nil {
		return err
	}
	storage, err := w.storage(ctx, nodes)
	if err != nil {
		return err
	}
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := w.Queries.WithTx(tx)
	row, err = q.LockEnvironment(ctx, row.ID)
	if err != nil {
		return err
	}
	if row.Revision != op.ExpectedRevision {
		return environment.ErrConflict
	}
	ids := []string{}
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	locked, err := q.LockNodes(ctx, ids)
	if err != nil {
		return err
	}
	byID := map[string]queries.Node{}
	for _, node := range locked {
		byID[node.ID] = node
	}
	for i := range nodes {
		latest := byID[nodes[i].ID]
		nodes[i].State, nodes[i].Info, nodes[i].CapacityOverride = latest.State, latest.Info, latest.CapacityOverride
	}
	reserved, err := q.GetNodeReservations(ctx, ids)
	if err != nil {
		return err
	}
	pools, err := q.ListStoragePools(ctx)
	if err != nil {
		return err
	}
	candidates, err := migrationCandidates(source, nodes, pools, reserved)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		if p.Migration.RequestedNode != nil && candidate.Id != *p.Migration.RequestedNode {
			continue
		}
		pool := storage[storageKey(candidate.Id, actualPool(source.NodeID, source.Execution))]
		if source.Execution.Rbd == nil || !pool.ready || pool.err != nil || pool.info.Rbd == nil {
			pool = storage[storageKey(candidate.Id, defaultStorage(candidate.Id))]
		}
		if !pool.ready || pool.err != nil {
			continue
		}
		target := source
		target.NodeID = candidate.Id
		assignStorage(&target.Execution, pool)
		target.Execution.Asset.StoragePoolId = target.Execution.StoragePoolId
		target.Execution.VolumeSources = map[string]api.NodeVolume{}
		live := candidate.Live && pool.info.Rbd != nil
		for name, volume := range source.Execution.VolumeSources {
			volumePool := pool
			if volume.Storage.Rbd != nil {
				shared := storage[storageKey(candidate.Id, volume.Storage.Rbd.SecretId)]
				if shared.err == nil && shared.ready && shared.info.Rbd != nil {
					volumePool = shared
				}
			}
			volume.Storage = volumePool.info
			live = live && volume.Storage.Rbd != nil
			target.Execution.VolumeSources[name] = volume
		}
		allocated, err := q.StorageReservations(ctx, ids)
		if err != nil {
			return err
		}
		used := map[string]int64{}
		for _, item := range allocated {
			id := item.PoolID
			if id == "" {
				id = defaultStorage(item.NodeID)
			}
			if entry, ok := storage[storageKey(item.NodeID, id)]; ok {
				used[entry.filesystem()] += item.DiskGib
			}
		}
		additional := int64(0)
		if pool.info.Rbd == nil {
			additional = resources(target.Execution.Asset).DiskGiB
		}
		for _, volume := range target.Execution.VolumeSources {
			if volume.Storage.Rbd == nil {
				additional += volume.SizeGiB
			}
		}
		if used[pool.filesystem()]+additional > pool.info.CapacityBytes>>30 || additional > pool.info.AvailableBytes>>30 {
			continue
		}
		p.Migration.Target = &target
		p.Migration.Live = live
		p.Targets = []Target{target}
		if !live {
			p.Recovery = &Recovery{EnvironmentID: row.ID, ID: op.ID, Assets: []Target{source}, Spec: p.Spec}
		}
		break
	}
	if p.Migration.Target == nil {
		return environment.Invalid("没有具备所需运行能力、存储和可用容量的迁移节点")
	}
	tw := w
	tw.Queries = q
	phase := "migration-prepare"
	if !p.Migration.Live {
		phase = "migration-stop"
	}
	if err = tw.phase(ctx, op, p, phase); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: %v", errPersistence, err)
	}
	return nil
}

func migrationRoute(env string, a api.AssetExecution) string {
	return "/node/v1/environments/" + env + "/assets/" + a.Asset.Id + "/instances/" + a.InstanceId + "/migration"
}

func (w Worker) migration(ctx context.Context, op *queries.Operation, p *Payload) error {
	if op.Phase == "queued" {
		if err := w.planMigration(ctx, op, p); err != nil {
			p.Before = nil
			p.Unchanged = nil
			return errors.Join(err, w.status(ctx, op, p, err))
		}
	}
	target := *p.Migration.Target
	source := findInstance(p.Before, target.Execution.InstanceId)
	nodes, err := w.Queries.GetNodeEndpoints(ctx, []string{source.NodeID, target.NodeID})
	if err != nil {
		return err
	}
	endpoints := map[string]string{}
	for _, node := range nodes {
		endpoints[node.ID] = node.Endpoint
	}
	for op.Phase != "complete" {
		next := ""
		switch op.Phase {
		case "migration-rollback", "rolled-back":
			return w.rollbackMigration(ctx, op, p, errors.New(*p.Failure), endpoints)
		case "migration-stop":
			if !matches(source.State, "stopped") {
				_, err = w.batch(ctx, op, p, api.NodePlanPhaseStop, []Target{source})
			}
			if err == nil {
				p.Recovery.Assets[0].State = "stopped"
			}
			next = "migration-capture"
		case "migration-capture":
			_, err = w.batch(ctx, op, p, api.NodePlanPhaseCaptureRecovery, []Target{source})
			next = "migration-copy"
		case "migration-copy":
			_, err = w.batch(ctx, op, p, api.NodePlanPhasePrepareRecovery, []Target{target})
			next = "migration-apply"
		case "migration-apply":
			_, err = w.batch(ctx, op, p, api.NodePlanPhaseApplyRecovery, []Target{target})
			next = "migration-activate"
		case "migration-activate":
			err = w.activate(ctx, op, p, []Target{target})
			next = "migration-verify"
		case "migration-prepare":
			var definition api.NodeVMMigration
			err = w.Client.Do(ctx, http.MethodGet, endpoints[source.NodeID], migrationRoute(*op.EnvironmentID, source.Execution), nil, &definition)
			if err == nil {
				input := api.NodeMigrationPreparation{EnvironmentId: *op.EnvironmentID, Execution: target.Execution, DomainXml: definition.DomainXml, SourceEndpoint: endpoints[source.NodeID]}
				err = w.Client.Do(ctx, http.MethodPost, endpoints[target.NodeID], "/node/v1/migrations/prepare", input, &definition)
				p.Migration.DomainXML = definition.DomainXml
			}
			next = "migration-transfer"
		case "migration-transfer":
			plan := api.NodePlan{OperationId: op.ID, EnvironmentId: *op.EnvironmentID, Phase: api.NodePlanPhaseMigrate, Spec: p.Spec, Assets: []api.AssetExecution{source.Execution}, Migrations: map[string]api.NodeVMMigration{*op.AssetID: {Endpoint: endpoints[target.NodeID], DomainXml: p.Migration.DomainXML}}}
			var result api.NodeResult
			result, err = w.Client.Execute(ctx, endpoints[source.NodeID], plan)
			if err == nil && result.Error != nil {
				err = errors.New(*result.Error)
			}
			if err == nil {
				if len(result.Results) != 1 || result.Results[0].InstanceId != source.Execution.InstanceId || result.Results[0].AssetId != *op.AssetID {
					err = errors.New("迁移结果与目标资产不一致")
				} else if result.Results[0].Error != nil {
					err = errors.New(*result.Results[0].Error)
				} else {
					p.Results = result.Results
				}
			}
			next = "migration-verify"
		case "migration-verify":
			var results []api.ExecutionResult
			results, err = w.batch(ctx, op, p, api.NodePlanPhasePolicies, []Target{target})
			if err == nil && (len(results) != 1 || !matches(results[0].State, source.State)) {
				err = errors.New("目标虚拟机尚未达到迁移前运行状态")
			}
			next = "migration-commit"
		case "migration-commit":
			err = w.commitMigration(ctx, op, p)
			next = "migration-cleanup"
			if !p.Migration.Live {
				next = "migration-release-capture"
			}
		case "migration-release-capture":
			_, err = w.batch(ctx, op, p, api.NodePlanPhaseDeleteRecovery, []Target{source})
			next = "migration-cleanup"
		case "migration-cleanup":
			err = w.Client.Do(ctx, http.MethodDelete, endpoints[source.NodeID], migrationRoute(*op.EnvironmentID, source.Execution), api.NodeMigrationCleanup{Source: source.Execution, Target: target.Execution}, nil)
			if err == nil && !p.Migration.Live {
				_, err = w.batch(ctx, op, p, api.NodePlanPhaseCleanupRecovery, []Target{target})
			}
			next = "migration-services"
		case "migration-services":
			p.Bindings, err = w.serviceRules(ctx, op, p, p.Spec, p.Bindings)
			if err == nil {
				err = commitServices(ctx, w.Queries, *op.EnvironmentID, p.Bindings)
			}
			if err == nil {
				_, err = w.batch(ctx, op, p, api.NodePlanPhasePolicies, []Target{target})
			}
			if err == nil {
				err = w.status(ctx, op, p, nil)
			}
			next = "complete"
		default:
			return fmt.Errorf("未知迁移阶段 %s", op.Phase)
		}
		if err != nil {
			if errors.Is(err, errPersistence) || ctx.Err() != nil {
				return err
			}
			detail := fmt.Errorf("%s: %w", op.Phase, err)
			var failure error = detail
			if !p.Committed {
				failure = w.rollbackMigration(ctx, op, p, detail, endpoints)
			}
			return errors.Join(failure, w.status(ctx, op, p, failure))
		}
		if op.Phase != next {
			if err = w.phase(ctx, op, p, next); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w Worker) rollbackMigration(ctx context.Context, op *queries.Operation, p *Payload, failure error, endpoints map[string]string) error {
	target := *p.Migration.Target
	source := findInstance(p.Before, target.Execution.InstanceId)
	if op.Phase == "rolled-back" {
		return errors.Join(failure, w.status(ctx, op, p, failure))
	}
	if op.Phase != "migration-rollback" {
		// The source must still own the instance; target cleanup refuses an active destination.
		results, err := w.batch(ctx, op, p, api.NodePlanPhaseInspect, []Target{source})
		if err != nil {
			return errors.Join(failure, err)
		}
		if len(results) != 1 || results[0].State != "running" && results[0].State != "suspended" && !matches(results[0].State, "stopped") {
			return errors.Join(failure, errors.New("迁移源实例归属尚未确认"))
		}
		if err = w.Client.Do(ctx, http.MethodDelete, endpoints[target.NodeID], migrationRoute(*op.EnvironmentID, target.Execution), api.NodeMigrationCleanup{Source: target.Execution, Target: source.Execution}, nil); err != nil {
			return errors.Join(failure, err)
		}
		message := failure.Error()
		p.Failure = &message
		if err = w.phase(ctx, op, p, "migration-rollback"); err != nil {
			return err
		}
	}
	if err := w.activate(ctx, op, p, []Target{source}); err != nil {
		return errors.Join(failure, err)
	}
	if !p.Migration.Live {
		if _, err := w.batch(ctx, op, p, api.NodePlanPhaseDeleteRecovery, []Target{source}); err != nil {
			return errors.Join(failure, err)
		}
	}
	if _, err := w.batch(ctx, op, p, api.NodePlanPhasePolicies, []Target{source}); err != nil {
		return errors.Join(failure, err)
	}
	if err := w.phase(ctx, op, p, "rolled-back"); err != nil {
		return err
	}
	return failure
}

func (w Worker) commitMigration(ctx context.Context, op *queries.Operation, p *Payload) error {
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
		return environment.ErrConflict
	}
	target := *p.Migration.Target
	source := findInstance(p.Before, target.Execution.InstanceId)
	result := p.Results[0]
	raw, err := json.Marshal(target.Execution)
	if err != nil {
		return err
	}
	count, err := q.CommitAssetMigration(ctx, queries.CommitAssetMigrationParams{InstanceID: target.Execution.InstanceId, NodeID: source.NodeID, NodeID_2: target.NodeID, Execution: raw, State: result.State, ObservedAt: pgtype.Timestamptz{Time: result.ObservedAt, Valid: true}})
	if err != nil {
		return err
	}
	if count != 1 {
		return environment.ErrConflict
	}
	for _, volume := range target.Execution.VolumeSources {
		pool := actualPool(target.NodeID, target.Execution)
		if volume.Storage.Rbd != nil {
			pool = volume.Storage.Rbd.SecretId
		}
		if err = q.MoveMigratedVolume(ctx, queries.MoveMigratedVolumeParams{ID: volume.Id, NodeID: target.NodeID, StoragePoolID: pool}); err != nil {
			return err
		}
	}
	if actualPool(source.NodeID, source.Execution) != actualPool(target.NodeID, target.Execution) {
		var desired api.EnvironmentSpec
		if err = json.Unmarshal(row.Spec, &desired); err != nil {
			return err
		}
		for _, spec := range []*api.EnvironmentSpec{&desired, &p.Spec} {
			for i := range spec.Assets {
				if spec.Assets[i].Id == *op.AssetID {
					spec.Assets[i].StoragePoolId = target.Execution.StoragePoolId
				}
			}
		}
		desiredRaw, err := json.Marshal(desired)
		if err != nil {
			return err
		}
		appliedRaw, err := json.Marshal(p.Spec)
		if err != nil {
			return err
		}
		if err = q.CommitMigrationSpec(ctx, queries.CommitMigrationSpecParams{ID: row.ID, Spec: desiredRaw, AppliedSpec: appliedRaw}); err != nil {
			return err
		}
	}
	p.Committed = true
	tw := w
	tw.Queries = q
	phase := "migration-cleanup"
	if !p.Migration.Live {
		phase = "migration-release-capture"
	}
	if err = tw.phase(ctx, op, p, phase); err != nil {
		p.Committed = false
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		p.Committed = false
		return fmt.Errorf("%w: %v", errPersistence, err)
	}
	return nil
}
