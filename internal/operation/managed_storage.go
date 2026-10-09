package operation

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/environment"
	"netlab.local/core/internal/transport"
)

// Node registration and successful configuration both use this single membership path.
func (s Service) ConfigureManagedStorage(ctx context.Context, replicas *int) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(73421493)"); err != nil {
		return err
	}
	q := s.Queries.WithTx(tx)
	nodes, err := q.ListNodes(ctx)
	if err != nil {
		return err
	}
	ids, owner := []string{}, ""
	devices := map[string]string{}
	for _, node := range nodes {
		if node.Retiring {
			if replicas != nil {
				return environment.ErrConflict
			}
			return nil
		}
		var info api.NodeInfo
		if err = json.Unmarshal(node.Info, &info); err != nil {
			return err
		}
		if node.State != "ready" || !slices.Contains(info.Capabilities, "vm") {
			continue
		}
		ids = append(ids, node.ID)
		if info.StorageDevice != nil {
			devices[node.ID] = *info.StorageDevice
		}
		if owner == "" && info.StorageDevice != nil {
			owner = node.ID
		}
	}
	pools, err := q.ListStoragePools(ctx)
	if err != nil {
		return err
	}
	var current *queries.ListStoragePoolsRow
	for i := range pools {
		if pools[i].Managed {
			current = &pools[i]
			break
		}
	}
	if current == nil && len(ids) < 2 {
		if replicas != nil {
			return environment.Invalid("共享存储需要两个就绪的 KVM 节点")
		}
		return nil
	}
	id, opID := uuid.NewString(), uuid.NewString()
	joining := slices.Clone(ids)
	if current != nil {
		if current.State != "ready" {
			if replicas != nil {
				return environment.ErrConflict
			}
			return nil
		}
		if current.OperationState != nil && *current.OperationState != "succeeded" {
			if replicas != nil {
				return environment.ErrConflict
			}
			return nil
		}
		id, owner = current.ID, current.NodeIds[0]
		// Keep registered members during temporary node outages.
		ids = append(slices.Clone(current.NodeIds), ids...)
		ids = uniqueMembers(owner, ids)
		previous, err := q.GetOperation(ctx, *current.OperationID)
		if err != nil {
			return err
		}
		var applied Payload
		if err = json.Unmarshal(previous.Payload, &applied); err != nil {
			return err
		}
		for _, node := range current.NodeIds {
			if _, present := devices[node]; !present {
				if device, used := applied.CephDevices[node]; used {
					devices[node] = device
				}
			}
		}
		if replicas == nil && slices.Equal(ids, current.NodeIds) && maps.Equal(devices, applied.CephDevices) {
			return nil
		}
		joining = slices.DeleteFunc(joining, func(node string) bool {
			return slices.Contains(current.NodeIds, node) && devices[node] == applied.CephDevices[node]
		})
	} else {
		if owner == "" {
			if replicas != nil {
				return environment.Invalid("请选择至少一块 Ceph 专用盘")
			}
			return nil
		}
		ids = uniqueMembers(owner, ids)
	}
	if replicas != nil && (*replicas < 1 || *replicas > 3 || *replicas > len(devices)) {
		return environment.Invalid("副本数不能超过存储节点数量，范围为 1 至 3")
	}
	payload, err := json.Marshal(Payload{StoragePool: &api.CreateStoragePool{NodeIds: ids, Name: "共享存储", Driver: api.StorageDriverRBD}, CephDevices: devices, CephReplicas: replicas, CephJoinNodes: joining})
	if err != nil {
		return err
	}
	if current == nil {
		err = q.CreateManagedStorage(ctx, queries.CreateManagedStorageParams{ID: id, NodeIds: ids, OperationID: &opID})
	} else {
		err = q.ConfigureManagedStorage(ctx, queries.ConfigureManagedStorageParams{ID: id, OperationID: &opID})
	}
	if err != nil {
		return err
	}
	if _, err = q.CreateOperation(ctx, queries.CreateOperationParams{ID: opID, ScopeKind: "storage-pool", ScopeID: id, Kind: "configure-storage-pool", Payload: payload}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func uniqueMembers(owner string, ids []string) []string {
	slices.Sort(ids)
	ids = slices.Compact(ids)
	return append([]string{owner}, slices.DeleteFunc(ids, func(id string) bool { return id == owner })...)
}

func (w Worker) configureStoragePool(ctx context.Context, op *queries.Operation, p *Payload) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	pool, err := w.Queries.GetStoragePool(ctx, op.ScopeID)
	if err != nil {
		return err
	}
	joining := p.CephJoinNodes
	if pool.Path == "" {
		joining = p.StoragePool.NodeIds
	}
	nodes, err := w.Queries.GetNodeEndpoints(ctx, p.StoragePool.NodeIds)
	if err != nil {
		return err
	}
	endpoints := map[string]string{}
	for _, node := range nodes {
		endpoints[node.ID] = node.Endpoint
	}
	owner := endpoints[p.StoragePool.NodeIds[0]]
	path := "/node/v1/ceph/" + op.ScopeID
	if err = w.phase(ctx, op, p, "storage-bootstrap"); err != nil {
		return err
	}
	var bootstrap api.NodeCephBootstrap
	if err = w.Client.Do(ctx, http.MethodPost, owner, path+"/bootstrap", nil, &bootstrap); err != nil {
		return err
	}
	if err = w.phase(ctx, op, p, "storage-join"); err != nil {
		return err
	}
	configuration := api.NodeCephConfiguration{Hosts: []api.NodeCephHost{}}
	configuration.Replicas = p.CephReplicas
	if pool.Path == "" {
		replicas := 1
		configuration.Replicas = &replicas
	}
	for _, id := range joining {
		var host api.NodeCephHost
		if err = w.Client.Do(ctx, http.MethodPost, endpoints[id], path+"/join", bootstrap, &host); err != nil {
			return err
		}
		configuration.Hosts = append(configuration.Hosts, host)
	}
	if err = w.phase(ctx, op, p, "storage-provision"); err != nil {
		return err
	}
	var connection api.CephConnection
	if err = w.Client.Do(ctx, http.MethodPost, owner, path+"/configure", configuration, &connection); err != nil {
		return err
	}
	// Credentials travel over mTLS and stay in the node keyrings, not in task payloads.
	input := *p.StoragePool
	input.Ceph = &connection
	if err = w.phase(ctx, op, p, "storage-register"); err != nil {
		return err
	}
	var info api.StorageInfo
	for _, id := range joining {
		if err = w.Client.Do(ctx, http.MethodPost, endpoints[id], transport.StorageRoute(op.ScopeID, ""), input, &info); err != nil {
			return err
		}
	}
	if pool.Path != "" {
		info.Path = pool.Path
	}
	return w.Queries.FinishManagedStorage(ctx, queries.FinishManagedStorageParams{ID: op.ScopeID, NodeIds: p.StoragePool.NodeIds, Path: info.Path})
}

func (w Worker) configureNodeStorage(ctx context.Context, op *queries.Operation, p *Payload) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	nodes, err := w.Queries.GetNodeEndpoints(ctx, []string{op.ScopeID})
	if err != nil {
		return err
	}
	if len(nodes) != 1 {
		return environment.Invalid("节点不存在")
	}
	if err = w.phase(ctx, op, p, "storage-node-preparation"); err != nil {
		return err
	}
	var info api.NodeInfo
	if err = w.Client.Do(ctx, http.MethodPut, nodes[0].Endpoint, "/node/v1/storage-device", p.StorageDevice, &info); err != nil {
		return err
	}
	raw, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return w.Queries.PutNode(ctx, queries.PutNodeParams{ID: op.ScopeID, Name: nodes[0].Name, Endpoint: nodes[0].Endpoint, Info: raw})
}
