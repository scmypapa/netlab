package operation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/environment"
	"netlab.local/core/internal/transport"
)

func (w Worker) NodeRetirement(ctx context.Context, id string) (api.NodeRetirement, error) {
	plan := api.NodeRetirement{Assets: []api.StoragePoolAsset{}, Blockers: []string{}}
	nodes, err := w.Queries.ListNodes(ctx)
	if err != nil {
		return plan, err
	}
	found, networkTarget := false, false
	endpoints := map[string]string{}
	devices := map[string]bool{}
	for _, node := range nodes {
		if node.ID == id {
			found = true
			plan.Draining = node.Retiring
		}
		var info api.NodeInfo
		if err = json.Unmarshal(node.Info, &info); err != nil {
			return plan, err
		}
		devices[node.ID] = info.StorageDevice != nil
		endpoints[node.ID] = node.Endpoint
		networkTarget = networkTarget || node.ID != id && node.State == "ready" && !node.Retiring && slices.Contains(info.Capabilities, "network")
	}
	if !found {
		return plan, pgx.ErrNoRows
	}
	plan.Blockers, err = w.Queries.NodeDependencies(ctx, id)
	if err != nil {
		return plan, err
	}
	assets, err := w.Queries.NodeAssets(ctx, id)
	if err != nil {
		return plan, err
	}
	for _, row := range assets {
		var execution api.AssetExecution
		if err = json.Unmarshal(row.Execution, &execution); err != nil {
			return plan, err
		}
		plan.Assets = append(plan.Assets, api.StoragePoolAsset{AssetId: row.AssetID, AssetName: execution.Asset.Name, EnvironmentId: row.EnvironmentID, EnvironmentName: row.EnvironmentName, NodeId: id, Revision: int64(row.Revision), SizeGiB: row.DiskGib, State: row.State})
		if !row.Current || execution.Asset.PciBinding != nil {
			plan.Blockers = append(plan.Blockers, "待处理资产："+execution.Asset.Name)
		}
	}
	owners, err := w.Queries.NodeNetworkEnvironments(ctx, &id)
	if err != nil {
		return plan, err
	}
	if len(owners) > 0 && !networkTarget {
		plan.Blockers = append(plan.Blockers, "没有可接替环境入口的网络节点")
	}
	pools, err := w.Queries.ListStoragePools(ctx)
	if err != nil {
		return plan, err
	}
	for _, pool := range pools {
		if pool.Driver != "rbd" || !slices.Contains(pool.NodeIds, id) {
			continue
		}
		if len(pool.NodeIds) < 2 {
			plan.Blockers = append(plan.Blockers, "共享池只有当前节点："+pool.Name)
			continue
		}
		if pool.State != "ready" || pool.OperationState != nil && *pool.OperationState != "succeeded" && pool.OperationID != nil {
			op, readErr := w.Queries.GetOperation(ctx, *pool.OperationID)
			if readErr != nil {
				return plan, readErr
			}
			if op.Kind != "retire-node" {
				plan.Blockers = append(plan.Blockers, "待完成存储操作："+pool.Name)
				continue
			}
		}
		if pool.Managed && devices[id] {
			remaining := 0
			for _, member := range pool.NodeIds {
				if member != id && devices[member] {
					remaining++
				}
			}
			var status api.CephStatus
			if err = w.Client.Do(ctx, http.MethodGet, endpoints[pool.NodeIds[0]], "/node/v1/ceph/"+pool.ID, nil, &status); err != nil {
				return plan, err
			}
			if remaining < status.Replicas {
				plan.Blockers = append(plan.Blockers, fmt.Sprintf("共享池需要保留 %d 个存储节点：%s", status.Replicas, pool.Name))
			}
		}
	}
	latest, err := w.Queries.NodeOperation(ctx, id)
	if err == nil && latest.Kind == "retire-node" {
		op, e := environment.Operation(latest)
		if e != nil {
			return plan, e
		}
		op.Retryable = plan.Draining && Retryable(access.Identity{Principal: queries.Principal{Kind: "user", Administrator: true}}, latest, queries.Environment{})
		plan.Operation = &op
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return plan, err
	}
	return plan, nil
}

func (w Worker) runRetirementChild(ctx context.Context, op *queries.Operation, p *Payload, id string) error {
	p.RetirementChild = &id
	if err := w.phase(ctx, op, p, op.Phase); err != nil {
		return err
	}
	owner := uuid.NewString()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		claimed, err := w.Queries.ClaimOperationByID(ctx, queries.ClaimOperationByIDParams{ID: id, Owner: &owner})
		if err == nil {
			w.execute(ctx, claimed)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		child, err := w.Queries.GetOperation(ctx, id)
		if err != nil {
			return err
		}
		if child.State == "succeeded" {
			p.RetirementChild = nil
			return w.phase(ctx, op, p, op.Phase)
		}
		if child.State == "failed" || child.State == "partially_applied" {
			if child.Error != nil {
				return fmt.Errorf("%s：%s", child.Kind, *child.Error)
			}
			return fmt.Errorf("%s：%s", child.Kind, child.Phase)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w Worker) retireNode(ctx context.Context, op *queries.Operation, p *Payload) error {
	if p.RetirementChild != nil {
		child, err := w.Queries.GetOperation(ctx, *p.RetirementChild)
		if err != nil {
			return err
		}
		if child.State == "failed" || child.State == "partially_applied" {
			service := Service{Pool: w.Pool, Queries: w.Queries}
			if _, err = service.Retry(ctx, access.Identity{Principal: queries.Principal{Kind: "user", Administrator: true}}, child.ID); err != nil {
				return err
			}
		}
		if err = w.runRetirementChild(ctx, op, p, child.ID); err != nil {
			return err
		}
	}
	if op.Phase == "queued" {
		plan, err := w.NodeRetirement(ctx, op.ScopeID)
		if err != nil {
			return err
		}
		if len(plan.Blockers) > 0 {
			return environment.Invalid("%s", plan.Blockers[0])
		}
		if err = w.phase(ctx, op, p, "retire-assets"); err != nil {
			return err
		}
	}
	service := Service{Pool: w.Pool, Queries: w.Queries}
	identity := access.Identity{Principal: queries.Principal{Kind: "user", Administrator: true}}
	if op.Phase == "retire-assets" {
		for {
			assets, err := w.Queries.NodeAssets(ctx, op.ScopeID)
			if err != nil {
				return err
			}
			if len(assets) == 0 {
				break
			}
			asset := assets[0]
			requestID := op.ID + "/" + asset.EnvironmentID + "/" + asset.AssetID
			child, err := service.Migrate(ctx, identity, asset.EnvironmentID, asset.AssetID, api.MigrationRequest{ExpectedRevision: int(asset.Revision), ClientRequestId: &requestID})
			if err != nil {
				return err
			}
			if err = w.runRetirementChild(ctx, op, p, child.Id); err != nil {
				return err
			}
		}
		if err := w.phase(ctx, op, p, "retire-network"); err != nil {
			return err
		}
	}
	if op.Phase == "retire-network" {
		owners, err := w.Queries.NodeNetworkEnvironments(ctx, &op.ScopeID)
		if err != nil {
			return err
		}
		for _, env := range owners {
			child, err := service.enqueueNetworkMove(ctx, env, op.ID)
			if err != nil {
				return err
			}
			if err = w.runRetirementChild(ctx, op, p, child.ID); err != nil {
				return err
			}
		}
		if err = w.phase(ctx, op, p, "retire-artifacts"); err != nil {
			return err
		}
	}
	if op.Phase == "retire-artifacts" {
		if err := w.relocateArtifacts(ctx, op.ScopeID); err != nil {
			return err
		}
		pools, err := w.Queries.ListStoragePools(ctx)
		if err != nil {
			return err
		}
		for _, pool := range pools {
			if pool.Driver == "rbd" && slices.Contains(pool.NodeIds, op.ScopeID) {
				p.CephJoinNodes = append(p.CephJoinNodes, pool.ID)
			}
		}
		if err = w.phase(ctx, op, p, "retire-storage"); err != nil {
			return err
		}
	}
	if op.Phase == "retire-storage" {
		if err := w.departStorage(ctx, op, p); err != nil {
			return err
		}
		if err := w.phase(ctx, op, p, "retire-storage-cleanup"); err != nil {
			return err
		}
	}
	if op.Phase == "retire-storage-cleanup" {
		nodes, err := w.Queries.GetNodeEndpoints(ctx, []string{op.ScopeID})
		if err != nil {
			return err
		}
		for _, id := range p.CephJoinNodes {
			pool, err := w.Queries.GetStoragePool(ctx, id)
			if err != nil {
				return err
			}
			if pool.Managed {
				if err = w.Client.Do(ctx, http.MethodDelete, nodes[0].Endpoint, "/node/v1/ceph/"+pool.ID, nil, nil); err != nil {
					return err
				}
			}
			if err = w.Client.Do(ctx, http.MethodDelete, nodes[0].Endpoint, "/node/v1/storage/"+pool.ID+"/registration", nil, nil); err != nil {
				return err
			}
		}
		if err = w.phase(ctx, op, p, "retire-complete"); err != nil {
			return err
		}
	}
	nodes, err := w.Queries.GetNodeEndpoints(ctx, []string{op.ScopeID})
	if err != nil {
		return err
	}
	return w.Client.Do(ctx, http.MethodDelete, nodes[0].Endpoint, "/node/v1/retirement", nil, nil)
}

func (w Worker) relocateArtifacts(ctx context.Context, source string) error {
	versions, err := w.Queries.NodeArtifactTemplates(ctx, source)
	if err != nil || len(versions) == 0 {
		return err
	}
	nodes, err := w.Queries.ListNodes(ctx)
	if err != nil {
		return err
	}
	var destination queries.ListNodesRow
	origin := ""
	for _, node := range nodes {
		if node.ID == source {
			origin = node.Endpoint
		}
	}
	for _, node := range nodes {
		if node.ID == source {
			continue
		}
		if node.State != "ready" || node.Retiring {
			continue
		}
		var info api.NodeInfo
		if err = json.Unmarshal(node.Info, &info); err != nil {
			return err
		}
		if slices.Contains(info.Capabilities, "vm") || slices.Contains(info.Capabilities, "container") {
			destination = node
			break
		}
	}
	if destination.ID == "" {
		return environment.Invalid("没有可接收模板制品的节点")
	}
	for _, raw := range versions {
		var template api.Template
		if err = json.Unmarshal(raw, &template); err != nil {
			return err
		}
		if err = w.Client.Do(ctx, http.MethodPost, destination.Endpoint, "/node/v1/templates/transfer", api.NodeTemplatePreparation{Template: template, ArtifactEndpoint: &origin}, nil); err != nil {
			return err
		}
	}
	return w.Queries.RelocateTemplateArtifacts(ctx, queries.RelocateTemplateArtifactsParams{Source: source, Target: destination.ID})
}

func (w Worker) departStorage(ctx context.Context, op *queries.Operation, p *Payload) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	pools, err := w.Queries.ListStoragePools(ctx)
	if err != nil {
		return err
	}
	nodes, err := w.Queries.ListNodes(ctx)
	if err != nil {
		return err
	}
	byID := map[string]queries.ListNodesRow{}
	for _, node := range nodes {
		byID[node.ID] = node
	}
	for _, pool := range pools {
		if !slices.Contains(pool.NodeIds, op.ScopeID) {
			continue
		}
		if pool.Driver == "directory" {
			refs, err := w.Queries.StorageReferences(ctx, pool.ID)
			if err != nil {
				return err
			}
			if len(refs) > 0 {
				return environment.Invalid("%s：%s", pool.Name, refs[0])
			}
			if err = w.Client.Do(ctx, http.MethodDelete, byID[op.ScopeID].Endpoint, transport.StorageRoute(pool.ID, pool.Directory), nil, nil); err != nil {
				return err
			}
			if err = w.Queries.DeleteStoragePool(ctx, pool.ID); err != nil {
				return err
			}
			continue
		}
		remaining := slices.DeleteFunc(slices.Clone(pool.NodeIds), func(id string) bool { return id == op.ScopeID })
		if len(remaining) == 0 {
			return environment.Invalid("共享池只有当前节点：%s", pool.Name)
		}
		owner := pool.NodeIds[0]
		if owner == op.ScopeID {
			owner = remaining[0]
		}
		if pool.Managed {
			var applied Payload
			previous, err := w.Queries.GetOperation(ctx, *pool.OperationID)
			if err != nil {
				return err
			}
			if err = json.Unmarshal(previous.Payload, &applied); err != nil {
				return err
			}
			p.CephDevices = applied.CephDevices
			delete(p.CephDevices, op.ScopeID)
			if _, ok := p.CephDevices[owner]; !ok {
				for _, id := range remaining {
					if _, ok = p.CephDevices[id]; ok {
						owner = id
						break
					}
				}
			}
			var admin api.NodeCephAdmin
			if err = w.Client.Do(ctx, http.MethodGet, byID[pool.NodeIds[0]].Endpoint, "/node/v1/ceph/"+pool.ID+"/admin", nil, &admin); err != nil {
				return err
			}
			if owner != pool.NodeIds[0] {
				if err = w.Client.Do(ctx, http.MethodPut, byID[owner].Endpoint, "/node/v1/ceph/"+pool.ID+"/admin", admin, nil); err != nil {
					return err
				}
			}
			departure := api.NodeCephDeparture{Remaining: []string{}}
			for _, id := range append(uniqueMembers(owner, remaining), op.ScopeID) {
				var host api.NodeCephHost
				if err = w.Client.Do(ctx, http.MethodPost, byID[id].Endpoint, "/node/v1/ceph/"+pool.ID+"/join", api.NodeCephBootstrap{PublicKey: admin.PublicKey}, &host); err != nil {
					return err
				}
				if id == op.ScopeID {
					departure.Host = host.Name
				} else if host.Device != nil {
					departure.Remaining = append(departure.Remaining, host.Name)
				}
			}
			if err = w.phase(ctx, op, p, op.Phase); err != nil {
				return err
			}
			if err = w.Queries.ConfigureManagedStorage(ctx, queries.ConfigureManagedStorageParams{ID: pool.ID, OperationID: &op.ID}); err != nil {
				return err
			}
			if err = w.Client.Do(ctx, http.MethodPost, byID[owner].Endpoint, "/node/v1/ceph/"+pool.ID+"/departure", departure, nil); err != nil {
				return err
			}
			var connection api.CephConnection
			if err = w.Client.Do(ctx, http.MethodPost, byID[owner].Endpoint, "/node/v1/ceph/"+pool.ID+"/configure", api.NodeCephConfiguration{Hosts: []api.NodeCephHost{}}, &connection); err != nil {
				return err
			}
			remaining = uniqueMembers(owner, remaining)
			for _, id := range remaining {
				input := api.CreateStoragePool{NodeIds: remaining, Driver: api.StorageDriverRBD, Name: pool.Name, Ceph: &connection}
				if err = w.Client.Do(ctx, http.MethodPost, byID[id].Endpoint, transport.StorageRoute(pool.ID, ""), input, nil); err != nil {
					return err
				}
			}
		}
		if err = w.Queries.FinishManagedStorage(ctx, queries.FinishManagedStorageParams{ID: pool.ID, NodeIds: remaining, Path: pool.Path}); err != nil {
			return err
		}
	}
	return nil
}
