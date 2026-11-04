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

	"golang.org/x/sync/errgroup"
	"netlab.local/core/api"
	"netlab.local/core/internal/access"
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
	parts := map[string]int64{}
	locations := map[string]api.StorageLocation{}
	partLocations := map[string]string{}
	var mu sync.Mutex
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(8)
	for _, node := range nodes {
		group.Go(func() error {
			var usage []api.StorageUsagePart
			if err := s.Nodes.Do(ctx, http.MethodPost, node.Endpoint, "/node/v1/storage-usage", requests[node.ID], &usage); err != nil {
				return fmt.Errorf("读取节点 %s 的存储占用：%w", node.Name, err)
			}
			mu.Lock()
			for _, part := range usage {
				parts[part.Key] = max(parts[part.Key], part.Bytes)
				location := api.StorageLocation{Id: node.ID, Name: node.Name}
				if strings.HasPrefix(part.Key, "rbd:") {
					location = api.StorageLocation{Id: "shared", Name: "共享存储"}
				}
				locations[location.Id] = location
				partLocations[part.Key] = location.Id
			}
			mu.Unlock()
			return nil
		})
	}
	if err = group.Wait(); err != nil {
		return api.EnvironmentStorage{}, err
	}
	result := api.EnvironmentStorage{MeasuredAt: time.Now().UTC(), Locations: []api.StorageLocation{}}
	for key, size := range parts {
		result.Bytes += size
		id := partLocations[key]
		location := locations[id]
		location.Bytes += size
		locations[id] = location
	}
	for _, location := range locations {
		result.Locations = append(result.Locations, location)
	}
	slices.SortFunc(result.Locations, func(a, b api.StorageLocation) int { return strings.Compare(a.Id, b.Id) })
	return result, nil
}
