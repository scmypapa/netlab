package operation

import (
	"context"
	"fmt"
	"net/http"
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
	if p.id != defaultStorage(p.node) {
		a.StoragePoolId = &p.id
	}
}

func (w Worker) deleteStoragePool(ctx context.Context, op *queries.Operation, p *Payload) error {
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
	return nil
}
