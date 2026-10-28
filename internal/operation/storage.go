package operation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/transport"
)

type storageCandidate struct {
	id, node, directory string
	info                api.StorageInfo
	container           *api.StorageInfo
	ready               bool
	err                 error
}

func (p storageCandidate) filesystem() string {
	if p.info.Rbd != nil {
		return p.info.Filesystem
	}
	return p.node + "/" + p.info.Filesystem
}
func defaultStorage(node string) string   { return "default:" + node }
func storageKey(node, pool string) string { return node + "/" + pool }

func (w Worker) storage(ctx context.Context, nodes []queries.ListNodesRow) (map[string]storageCandidate, error) {
	rows, err := w.Queries.ListStoragePools(ctx)
	if err != nil {
		return nil, err
	}
	byNode := make(map[string]queries.ListNodesRow, len(nodes))
	candidates := []storageCandidate{}
	for _, node := range nodes {
		byNode[node.ID] = node
		candidates = append(candidates, storageCandidate{id: defaultStorage(node.ID), node: node.ID, ready: node.State == "ready" && !node.Retiring})
	}
	for _, row := range rows {
		for _, node := range row.NodeIds {
			info := byNode[node]
			candidates = append(candidates, storageCandidate{id: row.ID, node: node, directory: row.Directory, ready: row.State == "ready" && info.State == "ready" && !info.Retiring})
		}
	}
	var wg sync.WaitGroup
	for i := range candidates {
		wg.Go(func() {
			p := &candidates[i]
			node := byNode[p.node]
			if node.State != "ready" {
				p.err = fmt.Errorf("存储节点 %s 未连接", p.node)
				return
			}
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if p.id == defaultStorage(p.node) {
				var info api.NodeInfo
				info, p.err = w.Client.Info(ctx, node.Endpoint)
				if p.err == nil {
					p.container = info.ContainerStorage
					if info.Storage == nil {
						p.err = fmt.Errorf("节点未提供存储信息")
					} else {
						p.info = *info.Storage
					}
				}
			} else {
				p.err = w.Client.Do(ctx, http.MethodGet, node.Endpoint, transport.StorageRoute(p.id, p.directory), nil, &p.info)
			}
		})
	}
	wg.Wait()
	result := map[string]storageCandidate{}
	shared := map[string]api.StorageInfo{}
	for _, p := range candidates {
		if p.err == nil && p.info.Rbd != nil {
			shared[p.id] = p.info
		}
	}
	for _, p := range candidates {
		if p.err != nil {
			if info, exists := shared[p.id]; exists {
				p.info = info
			}
		}
		result[storageKey(p.node, p.id)] = p
	}
	return result, nil
}

func storageUsage(rows []queries.StorageReservationsRow, storage map[string]storageCandidate) map[string]int64 {
	used := map[string]int64{}
	for _, row := range rows {
		pool := row.PoolID
		if pool == "" {
			pool = defaultStorage(row.NodeID)
		}
		if p, exists := storage[storageKey(row.NodeID, pool)]; exists {
			used[p.filesystem()] += row.DiskGib
		}
		if root := storage[storageKey(row.NodeID, defaultStorage(row.NodeID))].container; root != nil {
			used[row.NodeID+"/"+root.Filesystem] += row.ContainerGib
		}
	}
	return used
}

func storageLimits(storage map[string]storageCandidate) map[string]api.StorageInfo {
	limits := map[string]api.StorageInfo{}
	put := func(key string, info api.StorageInfo) {
		if previous, exists := limits[key]; exists {
			info.AvailableBytes = min(info.AvailableBytes, previous.AvailableBytes)
			info.CapacityBytes = min(info.CapacityBytes, previous.CapacityBytes)
		}
		limits[key] = info
	}
	for _, pool := range storage {
		if pool.err == nil {
			put(pool.filesystem(), pool.info)
		}
		if pool.container != nil {
			put(pool.node+"/"+pool.container.Filesystem, *pool.container)
		}
	}
	return limits
}

func storageAmounts(node string, kind api.TemplateKind, pool storageCandidate, budget diskBudget, storage map[string]storageCandidate) (map[string]int64, error) {
	amounts := map[string]int64{pool.filesystem(): budget.data}
	system := pool.filesystem()
	if kind == api.Container {
		root := storage[storageKey(node, defaultStorage(node))].container
		if root == nil {
			return nil, fmt.Errorf("节点未提供容器系统存储信息")
		}
		system = node + "/" + root.Filesystem
	}
	amounts[system] += budget.system
	return amounts, nil
}

func storageFits(used, pending map[string]int64, limits map[string]api.StorageInfo, additional map[string]int64) bool {
	for key, amount := range additional {
		info := limits[key]
		if used[key]+amount > info.CapacityBytes>>30 || pending[key]+amount > info.AvailableBytes>>30 {
			return false
		}
	}
	return true
}

func reserveStorage(used, pending, additional map[string]int64) {
	for key, amount := range additional {
		used[key] += amount
		pending[key] += amount
	}
}

func actualPool(node string, a api.AssetExecution) string {
	if a.StoragePoolId != nil {
		return *a.StoragePoolId
	}
	return defaultStorage(node)
}

func assignStorage(a *api.AssetExecution, p storageCandidate) {
	a.StoragePath, a.StorageFilesystem = &p.info.Path, &p.info.Filesystem
	a.Rbd = p.info.Rbd
	a.StoragePoolId = nil
	if p.id != defaultStorage(p.node) {
		a.StoragePoolId = &p.id
	}
}

func (w Worker) prepareSharedTemplates(ctx context.Context, targets []Target, artifacts map[string]string) error {
	pools, err := w.Queries.ListStoragePools(ctx)
	if err != nil {
		return err
	}
	nodes, err := w.Queries.ListNodes(ctx)
	if err != nil {
		return err
	}
	endpoints := map[string]string{}
	for _, node := range nodes {
		if node.State == "ready" {
			endpoints[node.ID] = node.Endpoint
		}
	}
	members := map[string][]string{}
	for _, pool := range pools {
		members[pool.ID] = pool.NodeIds
	}
	prepared := map[string]bool{}
	for _, target := range targets {
		a := target.Execution
		if a.Rbd == nil {
			continue
		}
		key := *a.StoragePoolId + "/" + a.Template.Id + fmt.Sprintf("/%d", a.Template.Version)
		if prepared[key] {
			continue
		}
		request := api.NodeTemplatePreparation{Template: a.Template, StoragePoolId: a.StoragePoolId}
		if a.Template.ArtifactNodeId != nil {
			endpoint := artifacts[*a.Template.ArtifactNodeId]
			request.ArtifactEndpoint = &endpoint
		}
		owner := ""
		for _, node := range members[*a.StoragePoolId] {
			if endpoints[node] != "" {
				owner = node
				break
			}
		}
		if a.Template.ArtifactNodeId != nil && slices.Contains(members[*a.StoragePoolId], *a.Template.ArtifactNodeId) && endpoints[*a.Template.ArtifactNodeId] != "" {
			owner = *a.Template.ArtifactNodeId
		}
		if owner == "" {
			return fmt.Errorf("共享存储没有可准备镜像的在线节点")
		}
		if err = w.prepareSharedTemplate(ctx, key, endpoints[owner], request); err != nil {
			return err
		}
		prepared[key] = true
	}
	return nil
}

func (w Worker) prepareSharedTemplate(ctx context.Context, key, endpoint string, request api.NodeTemplatePreparation) error {
	// The session lock spans node I/O; it must not occupy an API or lease-renewal connection.
	conn, err := pgx.ConnectConfig(ctx, w.Pool.Config().ConnConfig.Copy())
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn.Close(cleanup)
	}()
	if _, err = conn.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1, 0))", "shared-template/"+key); err != nil {
		return err
	}
	var prepared api.Template
	return w.Client.Do(ctx, http.MethodPost, endpoint, "/node/v1/templates/prepare", request, &prepared)
}

func (w Worker) deleteStoragePool(ctx context.Context, op *queries.Operation, p *Payload) error {
	pool, err := w.Queries.GetStoragePool(ctx, op.ScopeID)
	if err != nil {
		return err
	}
	nodes, err := w.Queries.GetNodeEndpoints(ctx, p.StoragePool.NodeIds)
	if err != nil {
		return err
	}
	if len(nodes) != len(p.StoragePool.NodeIds) {
		return fmt.Errorf("存储节点不存在")
	}
	if err = w.phase(ctx, op, p, "remove-storage-pool"); err != nil {
		return err
	}
	for _, node := range nodes {
		directory := ""
		if p.StoragePool.Directory != nil {
			directory = *p.StoragePool.Directory
		}
		if err = w.Client.Do(ctx, http.MethodDelete, node.Endpoint, transport.StorageRoute(op.ScopeID, directory), nil, nil); err != nil {
			return err
		}
	}
	if pool.Managed {
		members := map[string]queries.GetNodeEndpointsRow{}
		for _, node := range nodes {
			members[node.ID] = node
		}
		owner := pool.NodeIds[0]
		path := "/node/v1/ceph/" + pool.ID
		if err = w.phase(ctx, op, p, "storage-remove-cluster"); err != nil {
			return err
		}
		if err = w.Client.Do(ctx, http.MethodPost, members[owner].Endpoint, path+"/prepare-removal", nil, nil); err != nil {
			return err
		}
		for _, id := range append(slices.Clone(pool.NodeIds[1:]), owner) {
			node := members[id]
			if err = w.Client.Do(ctx, http.MethodDelete, node.Endpoint, path, nil, nil); err != nil {
				return err
			}
			info, err := w.Client.Info(ctx, node.Endpoint)
			if err != nil {
				return err
			}
			raw, err := json.Marshal(info)
			if err != nil {
				return err
			}
			if err = w.Queries.PutNode(ctx, queries.PutNodeParams{ID: id, Name: node.Name, Endpoint: node.Endpoint, Info: raw}); err != nil {
				return err
			}
		}
	}
	return nil
}
