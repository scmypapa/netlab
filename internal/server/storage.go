package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/environment"
	"netlab.local/core/internal/operation"
	"netlab.local/core/internal/transport"
)

func (s *Server) listStoragePools(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	nodes, err := s.Queries.ListNodes(r.Context())
	if err != nil {
		return err
	}
	pools, err := s.Queries.ListStoragePools(r.Context())
	if err != nil {
		return err
	}
	ids, endpoints := []string{}, map[string]string{}
	items := make([]api.StoragePool, 0, len(nodes)+len(pools))
	for _, node := range nodes {
		ids = append(ids, node.ID)
		endpoints[node.ID] = node.Endpoint
		items = append(items, api.StoragePool{Id: "default:" + node.ID, NodeIds: []string{node.ID}, Name: node.Name + " · 本地存储", Default: true, Driver: api.StorageDriverDirectory, Capabilities: []string{"vm-disks", "volumes"}})
	}
	for _, pool := range pools {
		state := api.StoragePoolState(pool.State)
		item := api.StoragePool{Id: pool.ID, NodeIds: pool.NodeIds, Name: pool.Name, Driver: api.StorageDriver(pool.Driver), State: &state, OperationId: pool.OperationID, Error: pool.OperationError, Capabilities: []string{"vm-disks", "volumes"}}
		if pool.Driver == "rbd" {
			item.Capabilities = []string{"vm-disks", "vm-volumes"}
		}
		if identity.Administrator() && pool.Driver == "directory" {
			item.Directory = &pool.Directory
		}
		items = append(items, item)
	}
	reservations, err := s.Queries.StorageReservations(r.Context(), ids)
	if err != nil {
		return err
	}
	for i := range items {
		for _, allocated := range reservations {
			if slices.Contains(items[i].NodeIds, allocated.NodeID) && (allocated.PoolID == items[i].Id || items[i].Default && allocated.PoolID == "") {
				items[i].AllocatedGiB += allocated.DiskGib
			}
		}
	}
	var wg sync.WaitGroup
	for i := range items {
		directory := ""
		if !items[i].Default {
			directory = pools[i-len(nodes)].Directory
		}
		wg.Go(func() {
			item := &items[i]
			var info api.StorageInfo
			var err error
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			if item.Default {
				var node api.NodeInfo
				node, err = s.Nodes.Info(ctx, endpoints[item.NodeIds[0]])
				if err == nil {
					if node.Storage == nil {
						err = fmt.Errorf("节点未提供存储信息")
					} else {
						info = *node.Storage
						if identity.Administrator() {
							item.Directory = &info.Path
						}
					}
				}
			} else {
				var failures error
				for _, node := range item.NodeIds {
					err = s.Nodes.Do(ctx, http.MethodGet, endpoints[node], transport.StorageRoute(item.Id, directory), nil, &info)
					if err == nil {
						break
					}
					failures = errors.Join(failures, err)
				}
				if err != nil {
					err = failures
				}
			}
			if err != nil {
				message := err.Error()
				item.Error, item.Storage = &message, nil
				return
			}
			if !identity.Administrator() {
				info.Path, info.Filesystem, info.Rbd = "", "", nil
			}
			item.Storage = &info
			if info.NativeSnapshots {
				item.Capabilities = append(item.Capabilities, "native-snapshots")
			}
		})
	}
	wg.Wait()
	return writeJSON(w, http.StatusOK, items)
}

func (s *Server) createStoragePool(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	var input api.CreateStoragePool
	if err := decode(w, r, &input); err != nil {
		return err
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.NodeIds) == 0 {
		return environment.Invalid("请填写存储池名称并选择节点")
	}
	slices.Sort(input.NodeIds)
	input.NodeIds = slices.Compact(input.NodeIds)
	nodes, err := s.Queries.GetNodeEndpoints(r.Context(), input.NodeIds)
	if err != nil {
		return err
	}
	if len(nodes) != len(input.NodeIds) {
		return httpError{http.StatusNotFound, "节点不存在"}
	}
	id := uuid.NewString()
	directory := ""
	if input.Directory != nil {
		directory = *input.Directory
	}
	path := transport.StorageRoute(id, directory)
	var info api.StorageInfo
	for _, node := range nodes {
		if err = s.Nodes.Do(r.Context(), http.MethodPost, node.Endpoint, path, input, &info); err != nil {
			break
		}
	}
	if err == nil {
		err = s.Queries.CreateStoragePool(r.Context(), queries.CreateStoragePoolParams{ID: id, NodeIds: input.NodeIds, Name: input.Name, Directory: directory, Path: info.Path, Driver: string(input.Driver)})
	}
	if err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, node := range nodes {
			if cleanup := s.Nodes.Do(ctx, http.MethodDelete, node.Endpoint, path, nil, nil); cleanup != nil {
				slog.Error("storage registration cleanup", "pool", id, "error", cleanup)
				err = errors.Join(err, cleanup)
			}
		}
		return err
	}
	state := api.StoragePoolStateReady
	capabilities := []string{"vm-disks", "volumes"}
	if input.Driver == api.StorageDriverRBD {
		capabilities = []string{"vm-disks", "vm-volumes", "native-snapshots"}
	}
	return writeJSON(w, http.StatusCreated, api.StoragePool{Id: id, NodeIds: input.NodeIds, Name: input.Name, Directory: input.Directory, Driver: input.Driver, State: &state, Storage: &info, Capabilities: capabilities})
}

func (s *Server) deleteStoragePool(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	ctx, id := r.Context(), r.PathValue("id")
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	pool, err := q.LockStoragePool(ctx, id)
	if err != nil {
		return err
	}
	if pool.State != "ready" {
		return httpError{http.StatusConflict, "请等待存储任务完成；失败任务可重试"}
	}
	refs, err := q.StorageReferences(ctx, id)
	if err != nil {
		return err
	}
	if len(refs) > 0 {
		return fmt.Errorf("%w：%s", environment.ErrInUse, strings.Join(refs, "、"))
	}
	opID := uuid.NewString()
	if err = q.MarkStorageDeleting(ctx, queries.MarkStorageDeletingParams{ID: id, OperationID: &opID}); err != nil {
		return err
	}
	payload, err := json.Marshal(operation.Payload{StoragePool: &api.CreateStoragePool{NodeIds: pool.NodeIds, Name: pool.Name, Directory: &pool.Directory, Driver: api.StorageDriver(pool.Driver)}})
	if err != nil {
		return err
	}
	op, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: opID, ScopeKind: "storage-pool", ScopeID: id, Kind: "delete-storage-pool", Payload: payload})
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	result, err := environment.Operation(op)
	if err != nil {
		return err
	}
	w.Header().Set("Operation-Location", "/api/v1/operations/"+opID)
	return writeJSON(w, http.StatusAccepted, result)
}
