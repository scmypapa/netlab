package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/environment"
	"netlab.local/core/internal/operation"
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
		items = append(items, api.StoragePool{Id: "default:" + node.ID, NodeId: node.ID, Name: node.Name + " · 本地存储", Default: true, Driver: api.Directory, Capabilities: []string{"vm-disks", "volumes"}})
	}
	for _, pool := range pools {
		state := api.StoragePoolState(pool.State)
		item := api.StoragePool{Id: pool.ID, NodeId: pool.NodeID, Name: pool.Name, Driver: api.Directory, State: &state, OperationId: pool.OperationID, Error: pool.OperationError, Storage: &api.StorageInfo{Path: pool.Path}, Capabilities: []string{"vm-disks", "volumes"}}
		if identity.Administrator() {
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
			if allocated.NodeID == items[i].NodeId && (allocated.PoolID == items[i].Id || items[i].Default && allocated.PoolID == "") {
				items[i].AllocatedGiB = allocated.DiskGib
			}
		}
	}
	var wg sync.WaitGroup
	for i := range items {
		wg.Go(func() {
			item := &items[i]
			var info api.StorageInfo
			var err error
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			if item.Default {
				var node api.NodeInfo
				node, err = s.Nodes.Info(ctx, endpoints[item.NodeId])
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
				err = s.Nodes.Do(ctx, http.MethodGet, endpoints[item.NodeId], "/node/v1/storage?path="+url.QueryEscape(item.Storage.Path), nil, &info)
			}
			if err != nil {
				message := err.Error()
				item.Error, item.Storage = &message, nil
				return
			}
			if !identity.Administrator() {
				info.Path, info.Filesystem = "", ""
			}
			item.Storage = &info
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
	if input.Name == "" {
		return environment.Invalid("请填写存储池名称")
	}
	nodes, err := s.Queries.GetNodeEndpoints(r.Context(), []string{input.NodeId})
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return httpError{http.StatusNotFound, "节点不存在"}
	}
	id := uuid.NewString()
	path := "/node/v1/storage/" + id
	var info api.StorageInfo
	if err = s.Nodes.Do(r.Context(), http.MethodPost, nodes[0].Endpoint, path, input, &info); err != nil {
		return err
	}
	if err = s.Queries.CreateStoragePool(r.Context(), queries.CreateStoragePoolParams{ID: id, NodeID: input.NodeId, Name: input.Name, Directory: input.Directory, Path: info.Path}); err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if cleanup := s.Nodes.Do(ctx, http.MethodDelete, nodes[0].Endpoint, path+"?directory="+url.QueryEscape(input.Directory), nil, nil); cleanup != nil {
			slog.Error("storage registration cleanup", "pool", id, "error", cleanup)
			return errors.Join(err, cleanup)
		}
		return err
	}
	state := api.StoragePoolStateReady
	return writeJSON(w, http.StatusCreated, api.StoragePool{Id: id, NodeId: input.NodeId, Name: input.Name, Directory: &input.Directory, Driver: api.Directory, State: &state, Storage: &info, Capabilities: []string{"vm-disks", "volumes"}})
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
	payload, err := json.Marshal(operation.Payload{StoragePool: &api.CreateStoragePool{NodeId: pool.NodeID, Name: pool.Name, Directory: pool.Directory}})
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
