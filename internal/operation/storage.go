package operation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/transport"
)

type storageCandidate struct {
	id, node, directory string
	info                api.StorageInfo
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
	endpoints := map[string]string{}
	candidates := []storageCandidate{}
	for _, node := range nodes {
		endpoints[node.ID] = node.Endpoint
		candidates = append(candidates, storageCandidate{id: defaultStorage(node.ID), node: node.ID, ready: node.State == "ready"})
	}
	for _, row := range rows {
		for _, node := range row.NodeIds {
			candidates = append(candidates, storageCandidate{id: row.ID, node: node, directory: row.Directory, ready: row.State == "ready"})
		}
	}
	var wg sync.WaitGroup
	for i := range candidates {
		wg.Go(func() {
			p := &candidates[i]
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if p.id == defaultStorage(p.node) {
				var info api.NodeInfo
				info, p.err = w.Client.Info(ctx, endpoints[p.node])
				if p.err == nil {
					if info.Storage == nil {
						p.err = fmt.Errorf("节点未提供存储信息")
					} else {
						p.info = *info.Storage
					}
				}
			} else {
				p.err = w.Client.Do(ctx, http.MethodGet, endpoints[p.node], transport.StorageRoute(p.id, p.directory), nil, &p.info)
			}
		})
	}
	wg.Wait()
	result := map[string]storageCandidate{}
	for _, p := range candidates {
		result[storageKey(p.node, p.id)] = p
	}
	return result, nil
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

func (w Worker) prepareSharedTemplate(ctx context.Context, key, endpoint string, request api.NodeTemplatePreparation) (err error) {
	conn, err := w.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	// Serialize publication across operations and nodes without a slow DB transaction.
	if _, err = conn.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1, 0))", "shared-template/"+key); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, unlockErr := conn.Exec(cleanup, "SELECT pg_advisory_unlock(hashtextextended($1, 0))", "shared-template/"+key)
		if unlockErr != nil {
			conn.Conn().Close(cleanup)
		}
		err = errors.Join(err, unlockErr)
	}()
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
