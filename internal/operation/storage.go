package operation

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
)

type storageCandidate struct {
	id, node string
	info     api.StorageInfo
	ready    bool
	err      error
}

func (p storageCandidate) filesystem() string { return p.node + "/" + p.info.Filesystem }
func defaultStorage(node string) string       { return "default:" + node }

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
		candidates = append(candidates, storageCandidate{id: row.ID, node: row.NodeID, info: api.StorageInfo{Path: row.Path}, ready: row.State == "ready"})
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
				p.err = w.Client.Do(ctx, http.MethodGet, endpoints[p.node], "/node/v1/storage?path="+url.QueryEscape(p.info.Path), nil, &p.info)
			}
		})
	}
	wg.Wait()
	result := map[string]storageCandidate{}
	for _, p := range candidates {
		result[p.id] = p
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
	if p.id != defaultStorage(p.node) {
		a.StoragePoolId = &p.id
	}
}

func (w Worker) deleteStoragePool(ctx context.Context, op *queries.Operation, p *Payload) error {
	nodes, err := w.Queries.GetNodeEndpoints(ctx, []string{p.StoragePool.NodeId})
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return fmt.Errorf("存储节点不存在")
	}
	if err = w.phase(ctx, op, p, "remove-storage-pool"); err != nil {
		return err
	}
	return w.Client.Do(ctx, http.MethodDelete, nodes[0].Endpoint, "/node/v1/storage/"+op.ScopeID+"?directory="+url.QueryEscape(p.StoragePool.Directory), nil, nil)
}
