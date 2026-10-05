package operation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/environment"
)

func (w Worker) SubmitVolume(ctx context.Context, id, kind string, input api.CreateVolume) (api.Operation, error) {
	nodes, err := w.Queries.ListNodes(ctx)
	if err != nil {
		return api.Operation{}, err
	}
	storage, err := w.storage(ctx, nodes)
	if err != nil {
		return api.Operation{}, err
	}
	var selected storageCandidate
	for _, node := range nodes {
		pool := storage[storageKey(node.ID, input.StoragePoolId)]
		if pool.ready && pool.err == nil && (input.Kind == api.Vm || pool.info.Rbd == nil) {
			selected = pool
			break
		}
	}
	if selected.id == "" {
		return api.Operation{}, environment.Invalid("没有可用的目标存储")
	}
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return api.Operation{}, err
	}
	defer tx.Rollback(ctx)
	q := w.Queries.WithTx(tx)
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, node.ID)
	}
	if _, err = q.LockNodes(ctx, ids); err != nil {
		return api.Operation{}, err
	}
	if !strings.HasPrefix(selected.id, "default:") {
		pools, err := q.LockStoragePools(ctx, []string{selected.id})
		if err != nil {
			return api.Operation{}, err
		}
		if len(pools) != 1 || pools[0].State != "ready" {
			return api.Operation{}, environment.Invalid("存储池正在删除")
		}
	}
	delta := input.SizeGiB
	if kind != "create-volume" {
		rows, err := q.LockPersistentVolumes(ctx, []string{id})
		if err != nil {
			return api.Operation{}, err
		}
		if len(rows) != 1 {
			return api.Operation{}, environment.Invalid("数据卷不存在")
		}
		row := rows[0]
		if row.State != "ready" && !(kind == "delete-volume" && row.State == "failed") {
			return api.Operation{}, environment.ErrInUse
		}
		refs, err := q.PersistentVolumeReferences(ctx, []string{id})
		if err != nil {
			return api.Operation{}, err
		}
		if len(refs) > 0 {
			names := make([]string, 0, len(refs))
			for _, ref := range refs {
				names = append(names, ref.Name)
			}
			return api.Operation{}, fmt.Errorf("%w：%s", environment.ErrInUse, strings.Join(names, "、"))
		}
		if kind == "resize-volume" && input.SizeGiB < row.SizeGib {
			return api.Operation{}, environment.Invalid("数据卷不能缩小")
		}
		input.Name, input.Kind, input.StoragePoolId = row.Name, api.TemplateKind(row.Kind), row.StoragePoolID
		delta -= row.SizeGib
	}
	if kind != "delete-volume" {
		allocated, err := q.StorageReservations(ctx, ids)
		if err != nil {
			return api.Operation{}, err
		}
		var used int64
		for _, row := range allocated {
			poolID := row.PoolID
			if poolID == "" {
				poolID = defaultStorage(row.NodeID)
			}
			pool := storage[storageKey(row.NodeID, poolID)]
			if pool.err == nil && pool.filesystem() == selected.filesystem() {
				used += row.DiskGib
			}
		}
		if used+delta > selected.info.CapacityBytes>>30 || delta > selected.info.AvailableBytes>>30 {
			return api.Operation{}, environment.Invalid("存储容量不足")
		}
	}
	opID := uuid.NewString()
	volume := api.NodeVolume{Id: id, Kind: input.Kind, SizeGiB: input.SizeGiB, Storage: selected.info}
	raw, err := json.Marshal(Payload{Volume: &volume, VolumeNode: selected.node})
	if err != nil {
		return api.Operation{}, err
	}
	op, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: opID, ScopeKind: "volume", ScopeID: id, Kind: kind, Payload: raw})
	if err != nil {
		return api.Operation{}, err
	}
	if kind == "create-volume" {
		err = q.CreatePersistentVolume(ctx, queries.CreatePersistentVolumeParams{ID: id, NodeID: selected.node, StoragePoolID: input.StoragePoolId, Name: input.Name, Kind: string(input.Kind), SizeGib: input.SizeGiB, OperationID: &opID})
	} else {
		state := "resizing"
		if kind == "delete-volume" {
			state = "deleting"
		}
		err = q.SetVolumeOperation(ctx, queries.SetVolumeOperationParams{ID: id, State: state, OperationID: &opID})
	}
	if err != nil {
		return api.Operation{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return api.Operation{}, err
	}
	return environment.Operation(op)
}

func (w Worker) volume(ctx context.Context, op *queries.Operation, p *Payload) error {
	nodes, err := w.Queries.GetNodeEndpoints(ctx, []string{p.VolumeNode})
	if err != nil {
		return err
	}
	if len(nodes) != 1 {
		return fmt.Errorf("数据卷节点不存在")
	}
	if err = w.phase(ctx, op, p, op.Kind); err != nil {
		return err
	}
	action := "prepare"
	if op.Kind == "delete-volume" {
		action = "delete"
	}
	return w.Client.Do(ctx, http.MethodPost, nodes[0].Endpoint, "/node/v1/volumes/"+action, p.Volume, nil)
}

func volumeBindings(a api.Asset, volumes map[string]queries.PersistentVolume, storage map[string]storageCandidate, node string) (map[string]api.NodeVolume, bool) {
	bindings := map[string]api.NodeVolume{}
	if a.Volumes != nil {
		for _, volume := range *a.Volumes {
			if volume.PersistentVolumeId == nil {
				continue
			}
			row := volumes[*volume.PersistentVolumeId]
			pool := storage[storageKey(node, row.StoragePoolID)]
			if !pool.ready || pool.err != nil || row.State != "ready" || a.StoragePoolId != nil && *a.StoragePoolId != row.StoragePoolID {
				return nil, false
			}
			bindings[volume.Id] = api.NodeVolume{Id: row.ID, Kind: api.TemplateKind(row.Kind), SizeGiB: row.SizeGib, Storage: pool.info}
		}
	}
	return bindings, true
}
