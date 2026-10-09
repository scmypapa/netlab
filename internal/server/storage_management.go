package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/environment"
	"netlab.local/core/internal/operation"
)

func (s *Server) nodeStorageDevices(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	nodes, err := s.Queries.GetNodeEndpoints(r.Context(), []string{r.PathValue("id")})
	if err != nil {
		return err
	}
	if len(nodes) != 1 {
		return httpError{http.StatusNotFound, "节点不存在"}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var result api.NodeStorageDevices
	if err = s.Nodes.Do(ctx, http.MethodGet, nodes[0].Endpoint, "/node/v1/storage-device", nil, &result); err != nil {
		return err
	}
	latest, err := s.Queries.NodeStorageOperation(ctx, nodes[0].ID)
	if err == nil {
		op, err := environment.Operation(latest)
		if err != nil {
			return err
		}
		result.Operation = &op
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) configureNodeStorage(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	var input api.ConfigureNodeStorage
	if err := decode(w, r, &input); err != nil {
		return err
	}
	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(r.Context())
	q := s.Queries.WithTx(tx)
	node, err := q.LockNode(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	if node.State != "ready" || node.Retiring {
		return environment.Invalid("节点当前离线")
	}
	latest, err := q.NodeStorageOperation(r.Context(), node.ID)
	if err == nil && (latest.State == "queued" || latest.State == "running") {
		return environment.ErrConflict
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	payload, err := json.Marshal(operation.Payload{StorageDevice: &input})
	if err != nil {
		return err
	}
	op, err := q.CreateOperation(r.Context(), queries.CreateOperationParams{ID: uuid.NewString(), ScopeKind: "node", ScopeID: node.ID, Kind: "configure-node-storage", Payload: payload})
	if err != nil {
		return err
	}
	if err = tx.Commit(r.Context()); err != nil {
		return err
	}
	result, err := environment.Operation(op)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) cephStatus(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	pool, err := s.Queries.GetStoragePool(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	if !pool.Managed {
		return environment.Invalid("此存储池由外部 Ceph 管理")
	}
	nodes, err := s.Queries.GetNodeEndpoints(r.Context(), []string{pool.NodeIds[0]})
	if err != nil {
		return err
	}
	if len(nodes) != 1 {
		return environment.Invalid("Ceph 管理节点不存在")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var result api.CephStatus
	if err = s.Nodes.Do(ctx, http.MethodGet, nodes[0].Endpoint, "/node/v1/ceph/"+pool.ID, nil, &result); err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) configureCephPool(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	pool, err := s.Queries.GetStoragePool(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	if !pool.Managed {
		return environment.Invalid("此存储池由外部 Ceph 管理")
	}
	var input api.ConfigureCephPool
	if err = decode(w, r, &input); err != nil {
		return err
	}
	if err = (operation.Service{Pool: s.Pool, Queries: s.Queries}).ConfigureManagedStorage(r.Context(), &input.Replicas); err != nil {
		return err
	}
	pool, err = s.Queries.GetStoragePool(r.Context(), pool.ID)
	if err != nil {
		return err
	}
	op, err := s.Queries.GetOperation(r.Context(), *pool.OperationID)
	if err != nil {
		return err
	}
	result, err := environment.Operation(op)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) storagePoolAssets(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	rows, err := s.Queries.StoragePoolAssets(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	result := make([]api.StoragePoolAsset, 0, len(rows))
	for _, row := range rows {
		result = append(result, api.StoragePoolAsset{EnvironmentId: row.EnvironmentID, EnvironmentName: row.EnvironmentName, Revision: int64(row.Revision), AssetId: row.AssetID, AssetName: row.AssetName, NodeId: row.NodeID, SizeGiB: row.SizeGib, State: row.State})
	}
	return writeJSON(w, http.StatusOK, result)
}
