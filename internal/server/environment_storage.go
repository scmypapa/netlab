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
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/operation"
)

func (s *Server) environmentStorage(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	id := r.PathValue("id")
	environment, err := s.Environments.Authorized(ctx, identity, id, "read", "")
	if err != nil {
		return err
	}
	result, _, err := s.storageReads.Do(ctx, id, func(ctx context.Context) (api.EnvironmentStorage, error) {
		return s.readEnvironmentStorage(ctx, id)
	})
	if err != nil {
		return err
	}
	result.Locations = slices.Clone(result.Locations)
	for i := range result.Locations {
		location := &result.Locations[i]
		var other int64
		visible := []api.StorageEnvironment{}
		for _, item := range location.Environments {
			if item.Id == id {
				item.Name = environment.Name
			} else if !identity.Administrator() {
				other += item.Bytes
				continue
			}
			visible = append(visible, item)
		}
		if other > 0 {
			visible = append(visible, api.StorageEnvironment{Id: "other", Name: "其他环境", Bytes: other})
		}
		location.Environments = visible
	}
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) readEnvironmentStorage(ctx context.Context, id string) (api.EnvironmentStorage, error) {
	sources, err := s.Queries.EnvironmentStorageSources(ctx, id)
	if err != nil {
		return api.EnvironmentStorage{}, err
	}
	requests := map[string]api.NodeStorageUsage{}
	sharedFilesystems := []string{}
	for _, source := range sources {
		request := requests[source.NodeID]
		request.EnvironmentId = id
		sourceUsage, err := storageSource(source.Execution, source.RecoveryID)
		if err != nil {
			return api.EnvironmentStorage{}, err
		}
		execution := sourceUsage.Execution
		if execution.Rbd != nil && execution.StorageFilesystem != nil {
			sharedFilesystems = append(sharedFilesystems, *execution.StorageFilesystem)
		}
		request.Sources = append(request.Sources, sourceUsage)
		requests[source.NodeID] = request
	}
	ids := make([]string, 0, len(requests))
	for id := range requests {
		ids = append(ids, id)
	}
	namesByEnvironment := map[string]string{id: "本环境"}
	others, err := s.Queries.OtherEnvironmentStorageSources(ctx, queries.OtherEnvironmentStorageSourcesParams{EnvironmentID: id, NodeIds: ids, SharedFilesystems: sharedFilesystems})
	if err != nil {
		return api.EnvironmentStorage{}, err
	}
	for _, source := range others {
		request, exists := requests[source.NodeID]
		if !exists {
			ids = append(ids, source.NodeID)
			request.EnvironmentId = id
		}
		if request.Environments == nil {
			request.Environments = &map[string][]api.StorageUsageSource{}
		}
		sourceUsage, err := storageSource(source.Execution, source.RecoveryID)
		if err != nil {
			return api.EnvironmentStorage{}, err
		}
		(*request.Environments)[source.EnvironmentID] = append((*request.Environments)[source.EnvironmentID], sourceUsage)
		namesByEnvironment[source.EnvironmentID] = source.Name
		requests[source.NodeID] = request
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
				if part.EnvironmentId == "" {
					part.EnvironmentId = id
				}
				previous, ok := parts[part.Key]
				sameOwner := part.EnvironmentId == previous.EnvironmentId
				earlierOwner := part.EnvironmentId == id || previous.EnvironmentId != id && part.EnvironmentId < previous.EnvironmentId
				if !ok || sameOwner && part.Bytes >= previous.Bytes || !sameOwner && earlierOwner {
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
	occupants := map[string]map[string]int64{}
	for _, part := range parts {
		location := locations[part.Filesystem]
		if part.EnvironmentId == id {
			result.Bytes += part.Bytes
			result.BaseBytes += part.BaseBytes
			location.Bytes += part.Bytes
			location.BaseBytes += part.BaseBytes
		}
		if occupants[part.Filesystem] == nil {
			occupants[part.Filesystem] = map[string]int64{}
		}
		occupants[part.Filesystem][part.EnvironmentId] += part.Bytes
		locations[part.Filesystem] = location
	}
	for fs, location := range locations {
		location.Id, location.Name = fs, names[fs]
		if location.Bytes == 0 {
			continue
		}
		location.Environments = []api.StorageEnvironment{}
		for owner, bytes := range occupants[fs] {
			location.Environments = append(location.Environments, api.StorageEnvironment{Id: owner, Name: namesByEnvironment[owner], Bytes: bytes})
		}
		slices.SortFunc(location.Environments, func(a, b api.StorageEnvironment) int { return strings.Compare(a.Id, b.Id) })
		location.CapacityBytes = limits[fs].CapacityBytes
		location.AvailableBytes = max(int64(0), operation.AvailableStorageBytes(limits[fs])-pending[fs])
		result.Locations = append(result.Locations, location)
	}
	slices.SortFunc(result.Locations, func(a, b api.StorageLocation) int { return strings.Compare(a.Id, b.Id) })
	return result, nil
}

// Storage queries carry disk identities and volume references; guest files and network settings remain on the execution path.
func storageSource(raw []byte, recoveryID string) (api.StorageUsageSource, error) {
	var a api.AssetExecution
	if err := json.Unmarshal(raw, &a); err != nil {
		return api.StorageUsageSource{}, err
	}
	a.Asset = api.Asset{Id: a.Asset.Id, Volumes: a.Asset.Volumes}
	a.Template = api.Template{Id: a.Template.Id, Version: a.Template.Version, Kind: a.Template.Kind, Disks: a.Template.Disks, Media: a.Template.Media, StateFiles: a.Template.StateFiles}
	a.Interfaces = nil
	a.PciDevices = nil
	a.PreviousInstanceId = nil
	return api.StorageUsageSource{Execution: a, RecoveryId: recoveryID}, nil
}
