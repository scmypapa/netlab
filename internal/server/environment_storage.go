package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"
	"netlab.local/core/api"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/operation"
)

func (s *Server) environmentStorage(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	id := r.PathValue("id")
	if _, err := s.Environments.Authorized(ctx, identity, id, "read", ""); err != nil {
		return err
	}
	result, _, err := s.storageReads.Do(ctx, id, func(ctx context.Context) (api.EnvironmentStorage, error) {
		return s.readEnvironmentStorage(ctx, id)
	})
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) readEnvironmentStorage(ctx context.Context, id string) (api.EnvironmentStorage, error) {
	sources, err := s.Queries.EnvironmentStorageSources(ctx, id)
	if err != nil {
		return api.EnvironmentStorage{}, err
	}
	requests := map[string]api.NodeStorageUsage{}
	for _, source := range sources {
		request := requests[source.NodeID]
		request.EnvironmentId = id
		var execution api.AssetExecution
		if err = json.Unmarshal(source.Execution, &execution); err != nil {
			return api.EnvironmentStorage{}, err
		}
		request.Sources = append(request.Sources, api.StorageUsageSource{Execution: execution, RecoveryId: source.RecoveryID})
		requests[source.NodeID] = request
	}
	ids := make([]string, 0, len(requests))
	for id := range requests {
		ids = append(ids, id)
	}
	nodes, err := s.Queries.GetNodeEndpoints(ctx, ids)
	if err != nil {
		return api.EnvironmentStorage{}, err
	}
	if len(nodes) != len(ids) {
		return api.EnvironmentStorage{}, fmt.Errorf("环境存储节点未登记，请检查节点管理")
	}
	observed := time.Now()
	parts := map[string]api.StorageUsagePart{}
	limits := map[string]api.StorageInfo{}
	names := map[string]string{}
	pools, err := s.Queries.ListStoragePools(ctx)
	if err != nil {
		return api.EnvironmentStorage{}, err
	}
	poolNames := map[string]string{}
	for _, pool := range pools {
		poolNames[pool.ID] = pool.Name
	}
	for _, node := range nodes {
		for _, source := range requests[node.ID].Sources {
			a := source.Execution
			if a.StorageFilesystem == nil {
				continue
			}
			fs, name := node.ID+"/"+*a.StorageFilesystem, node.Name
			if a.Rbd != nil {
				fs = *a.StorageFilesystem
				name = "共享存储"
			}
			if a.StoragePoolId != nil {
				name = poolNames[*a.StoragePoolId]
			}
			names[fs] = name
		}
	}
	var mu sync.Mutex
	group, nodeCtx := errgroup.WithContext(ctx)
	group.SetLimit(8)
	for _, node := range nodes {
		group.Go(func() error {
			var usage []api.StorageUsagePart
			if err := s.Nodes.Do(nodeCtx, http.MethodPost, node.Endpoint, "/node/v1/storage-usage", requests[node.ID], &usage); err != nil {
				return fmt.Errorf("读取节点 %s 的存储占用：%w", node.Name, err)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, part := range usage {
				if part.Storage == nil || part.Filesystem == "" {
					return fmt.Errorf("节点 %s 未提供完整存储统计，请更新节点", node.Name)
				}
				if previous, ok := parts[part.Key]; !ok || part.Bytes >= previous.Bytes {
					parts[part.Key] = part
				}
				info := *part.Storage
				if previous, ok := limits[part.Filesystem]; ok {
					info.AvailableBytes = min(info.AvailableBytes, previous.AvailableBytes)
					info.CapacityBytes = min(info.CapacityBytes, previous.CapacityBytes)
				}
				limits[part.Filesystem] = info
				if names[part.Filesystem] == "" {
					names[part.Filesystem] = node.Name
					if info.Rbd != nil {
						names[part.Filesystem] = info.Rbd.Pool
					}
				}
			}
			return nil
		})
	}
	if err = group.Wait(); err != nil {
		return api.EnvironmentStorage{}, err
	}
	writes, err := s.Queries.PendingStorageWrites(ctx, pgtype.Timestamptz{Time: observed, Valid: true})
	if err != nil {
		return api.EnvironmentStorage{}, err
	}
	pending := map[string]int64{}
	for _, write := range writes {
		pending[write.Filesystem] = write.Gib << 30
	}
	result := api.EnvironmentStorage{MeasuredAt: time.Now().UTC(), Locations: []api.StorageLocation{}}
	locations := map[string]api.StorageLocation{}
	for _, part := range parts {
		result.Bytes += part.Bytes
		result.BaseBytes += part.BaseBytes
		location := locations[part.Filesystem]
		location.Bytes += part.Bytes
		location.BaseBytes += part.BaseBytes
		locations[part.Filesystem] = location
	}
	for fs, location := range locations {
		location.Id, location.Name = fs, names[fs]
		location.CapacityBytes = limits[fs].CapacityBytes
		location.AvailableBytes = max(int64(0), operation.AvailableStorageBytes(limits[fs])-pending[fs])
		result.Locations = append(result.Locations, location)
	}
	slices.SortFunc(result.Locations, func(a, b api.StorageLocation) int { return strings.Compare(a.Id, b.Id) })
	return result, nil
}
