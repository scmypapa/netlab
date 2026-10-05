package operation

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/google/uuid"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/environment"
)

type reservation struct {
	EnvironmentID string             `json:"environmentId"`
	AssetID       string             `json:"assetId"`
	InstanceID    string             `json:"instanceId"`
	NodeID        string             `json:"nodeId"`
	Execution     api.AssetExecution `json:"execution"`
	api.Resources
}

func resourceRecords(id string, targets []Target) ([]byte, error) {
	rows := make([]reservation, 0, len(targets))
	for _, t := range targets {
		r := resources(t.Execution.Asset)
		rows = append(rows, reservation{id, t.Execution.Asset.Id, t.Execution.InstanceId, t.NodeID, t.Execution, r})
	}
	return json.Marshal(rows)
}

func recoveryReservations(targets, before []Target) []Target {
	allocated := slices.Clone(targets)
	for i := range allocated {
		old := findInstance(before, allocated[i].Execution.InstanceId)
		allocated[i].Execution.Asset.Resources.DiskGiB += resources(old.Execution.Asset).DiskGiB
	}
	return allocated
}

func (w Worker) plan(ctx context.Context, op *queries.Operation, p *Payload) error {
	restoring := restoresData(op.Kind)
	sources := map[string]Target{}
	if restoring {
		p.Spec.Assets = slices.Clone(p.Spec.Assets)
		for i := range p.Spec.Assets {
			if p.Spec.Assets[i].Volumes == nil {
				continue
			}
			copies := slices.Clone(*p.Spec.Assets[i].Volumes)
			for j := range copies {
				copies[j].PersistentVolumeId = nil
			}
			p.Spec.Assets[i].Volumes = &copies
		}
		for _, source := range p.Recovery.Assets {
			sources[source.Execution.Asset.Id] = source
		}
	}
	row, err := w.Queries.GetEnvironment(ctx, *op.EnvironmentID)
	if err != nil {
		return err
	}
	if row.Revision != op.ExpectedRevision {
		return environment.ErrConflict
	}
	nodes, err := w.Queries.ListNodes(ctx)
	if err != nil {
		return err
	}
	byNode := map[string]queries.ListNodesRow{}
	infos := map[string]api.NodeInfo{}
	p.ExternalChassis = map[string]string{}
	for _, n := range nodes {
		byNode[n.ID] = n
		var info api.NodeInfo
		if err = json.Unmarshal(n.Info, &info); err != nil {
			return err
		}
		infos[n.ID] = info
		if info.NetworkChassis != nil {
			p.ExternalChassis[n.ID] = *info.NetworkChassis
		}
	}
	if len(row.AppliedSpec) > 0 && !(restoring && p.BeforeStatus == "destroyed") {
		if err = json.Unmarshal(row.AppliedSpec, &p.BeforeSpec); err != nil {
			return err
		}
	}
	serviceOnly := op.Kind == "change" && p.BeforeSpec != nil && environment.ServiceOnly(*p.BeforeSpec, p.Spec)
	actual := []queries.RuntimeAsset{}
	if !serviceOnly {
		actual, err = w.Queries.ListRuntimeAssets(ctx, row.ID)
		if err != nil {
			return err
		}
	}
	current := map[string]Target{}
	for _, a := range actual {
		_, ok := byNode[a.NodeID]
		if !ok {
			return fmt.Errorf("资产 %s 的运行节点不存在", a.AssetID)
		}
		var execution api.AssetExecution
		if err = json.Unmarshal(a.Execution, &execution); err != nil {
			return err
		}
		t := Target{NodeID: a.NodeID, Execution: execution, State: a.State}
		if a.Current {
			current[a.AssetID] = t
			p.Before = append(p.Before, t)
		} else {
			p.Old = append(p.Old, t)
		}
	}
	for _, n := range nodes {
		if row.NetworkNodeID != nil && n.ID == *row.NetworkNodeID {
			p.Owner = &Target{NodeID: n.ID}
			break
		}
		if p.Owner == nil && n.State == "ready" && slices.Contains(infos[n.ID].Capabilities, "network") && (len(environment.Services(p.Spec)) == 0 || infos[n.ID].ServiceNetwork != nil) {
			p.Owner = &Target{NodeID: n.ID}
		}
	}
	if p.Owner == nil && len(p.Spec.Networks) > 0 {
		return fmt.Errorf("没有可运行虚拟网络的节点")
	}
	if err = w.loadServices(ctx, row, p); err != nil {
		return err
	}
	if op.Kind == "destroy" && op.AssetID == nil {
		p.Targets = p.Before
		phase := "destroy"
		if p.Gateway != nil && p.BeforeSpec != nil {
			phase = "remove-services"
		}
		return w.phase(ctx, op, p, phase)
	}
	if op.Kind != "change" && op.Kind != "rebuild" && op.Kind != "start" && op.Kind != "destroy" && !restoring {
		for _, t := range p.Before {
			if op.AssetID == nil || t.Execution.Asset.Id == *op.AssetID {
				p.Targets = append(p.Targets, t)
			} else {
				p.Unchanged = append(p.Unchanged, t)
			}
		}
		return w.phase(ctx, op, p, "control")
	}
	if op.Kind == "destroy" {
		before := p.Spec
		p.Spec.Assets = slices.DeleteFunc(p.Spec.Assets, func(a api.Asset) bool { return a.Id == *op.AssetID })
		for i := range p.Spec.Networks {
			if p.Spec.Networks[i].DnsAssetId != nil && *p.Spec.Networks[i].DnsAssetId == *op.AssetID {
				p.Spec.Networks[i].DnsAssetId = nil
			}
		}
		p.Spec = environment.RemoveDependentServices(before, p.Spec)
	}
	if serviceOnly {
		p.Unchanged = p.Before
	} else {
		externalNodes := map[string]bool{}
		for _, nw := range p.Spec.Networks {
			if nw.External != nil {
				externalNodes[nw.External.NodeId] = true
			}
		}
		for id := range externalNodes {
			node, exists := byNode[id]
			if !exists {
				return fmt.Errorf("外部网络节点不存在")
			}
			info, err := w.Client.Info(ctx, node.Endpoint)
			if err != nil {
				return err
			}
			if info.Id != id {
				return fmt.Errorf("外部网络节点身份不匹配")
			}
			infos[id] = info
			if info.NetworkChassis != nil {
				p.ExternalChassis[id] = *info.NetworkChassis
			}
		}
		templates := map[string]api.Template{}
		if !restoring {
			templates, err = environment.Templates(ctx, w.Queries, p.Spec.Assets)
			if err != nil {
				return err
			}
		}
		for _, a := range p.Spec.Assets {
			t, ok := templates[a.TemplateId]
			if restoring {
				source, exists := sources[a.Id]
				if !exists {
					return fmt.Errorf("恢复点缺少资产 %s", a.Name)
				}
				t, ok = source.Execution.Template, true
			}
			if !ok || !restoring && (t.State == nil || *t.State != api.TemplateStateReady) {
				return fmt.Errorf("资产 %s 的模板尚未准备完成", a.Name)
			}
			old, exists := current[a.Id]
			delete(current, a.Id)
			nics := []api.ResolvedInterface{}
			state := "running"
			if exists {
				nics = old.Execution.Interfaces
				state = old.State
			} else if p.BeforeStatus == "stopped" || p.BeforeStatus == "suspended" {
				state = p.BeforeStatus
			}
			if op.Kind == "start" && (op.AssetID == nil || *op.AssetID == a.Id) {
				state = "running"
			}
			execution := api.AssetExecution{Asset: a, Template: t, InstanceId: uuid.NewString(), Interfaces: environment.Resolve(p.Spec, a, nics)}
			if exists {
				execution.StoragePoolId, execution.StoragePath, execution.StorageFilesystem = old.Execution.StoragePoolId, old.Execution.StoragePath, old.Execution.StorageFilesystem
				execution.Rbd = old.Execution.Rbd
				execution.DataSetId = old.Execution.DataSetId
			}
			if restoring {
				source := sources[a.Id]
				execution.DataSetId = op.ID
				if op.Kind == "restore-recovery" {
					execution.InstanceId = source.Execution.InstanceId
					execution.Interfaces = environment.Resolve(p.Spec, a, source.Execution.Interfaces)
				}
				target := Target{Execution: execution, State: source.State}
				if op.Kind == "clone-recovery" {
					target.State = "stopped"
					if p.Run {
						target.State = "running"
					}
				}
				if exists && old.Execution.InstanceId == execution.InstanceId {
					target.NodeID = old.NodeID
					p.Updates = append(p.Updates, target)
				} else {
					if exists {
						p.Old = append(p.Old, old)
					}
					p.Targets = append(p.Targets, target)
				}
				continue
			}
			replace := exists && (environment.RequiresReplacement(t, old.Execution.Asset, a, !reflect.DeepEqual(old.Execution.Interfaces, execution.Interfaces)) || old.Execution.Template.Version != t.Version || op.Kind == "rebuild" && (op.AssetID == nil || *op.AssetID == a.Id))
			if !exists || replace {
				if replace {
					previous := old.Execution.InstanceId
					execution.PreviousInstanceId = &previous
					p.Old = append(p.Old, old)
				}
				p.Targets = append(p.Targets, Target{Execution: execution, State: state})
				continue
			}
			execution.InstanceId = old.Execution.InstanceId
			if !reflect.DeepEqual(old.Execution.Asset, a) || !reflect.DeepEqual(old.Execution.Interfaces, execution.Interfaces) {
				p.Updates = append(p.Updates, Target{NodeID: old.NodeID, Execution: execution, State: state})
			} else {
				old.State = state
				p.Unchanged = append(p.Unchanged, old)
			}
		}
		for _, t := range current {
			p.Old = append(p.Old, t)
		}
	}
	storage := map[string]storageCandidate{}
	if len(p.Targets)+len(p.Updates) > 0 {
		if storage, err = w.storage(ctx, nodes); err != nil {
			return err
		}
	}
	// Requirements are immutable here; locks cover only fresh capacity and reservations.
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := w.Queries.WithTx(tx)
	lockedEnvironment, err := q.LockEnvironment(ctx, row.ID)
	if err != nil {
		return err
	}
	if lockedEnvironment.Revision != op.ExpectedRevision {
		return environment.ErrConflict
	}
	ids := make([]string, 0, len(nodes))
	if serviceOnly && p.Owner != nil {
		ids = append(ids, p.Owner.NodeID)
	} else {
		for _, n := range nodes {
			ids = append(ids, n.ID)
		}
	}
	locked, err := q.LockNodes(ctx, ids)
	if err != nil {
		return err
	}
	volumeRows, err := q.LockPersistentVolumes(ctx, environment.PersistentVolumeIDs(p.Spec.Assets))
	if err != nil {
		return err
	}
	volumes := map[string]queries.PersistentVolume{}
	for _, volume := range volumeRows {
		volumes[volume.ID] = volume
	}
	if err = environment.CheckVolumeUses(ctx, q, p.Spec.Assets, row.ID); err != nil {
		return err
	}
	diskUsed, diskCapacity := map[string]int64{}, map[string]int64{}
	if len(storage) > 0 {
		poolIDs := []string{}
		for _, pool := range storage {
			if pool.id != defaultStorage(pool.node) && !slices.Contains(poolIDs, pool.id) {
				poolIDs = append(poolIDs, pool.id)
			}
		}
		pools, err := q.LockStoragePools(ctx, poolIDs)
		if err != nil {
			return err
		}
		poolReady := map[string]bool{}
		for _, pool := range pools {
			poolReady[pool.ID] = pool.State == "ready"
		}
		for id, pool := range storage {
			if pool.id != defaultStorage(pool.node) {
				pool.ready = poolReady[pool.id]
				storage[id] = pool
			}
		}
		allocated, err := q.StorageReservations(ctx, ids)
		if err != nil {
			return err
		}
		for _, r := range allocated {
			id := r.PoolID
			if id == "" {
				id = defaultStorage(r.NodeID)
			}
			if pool, ok := storage[storageKey(r.NodeID, id)]; ok && pool.err == nil {
				diskUsed[pool.filesystem()] += r.DiskGib
			}
		}
		for _, pool := range storage {
			if pool.err == nil {
				key := pool.filesystem()
				diskCapacity[key] = pool.info.CapacityBytes >> 30
			}
		}
	}
	capacity, used := map[string]api.Resources{}, map[string]api.Resources{}
	reservations, err := q.GetNodeReservations(ctx, ids)
	if err != nil {
		return err
	}
	for _, r := range reservations {
		used[r.NodeID] = api.Resources{Cpu: int(r.Cpu), MemoryMiB: r.MemoryMib, DiskGiB: r.DiskGib}
	}
	deviceRows, err := q.DeviceReservations(ctx, ids)
	if err != nil {
		return err
	}
	occupiedDevices := map[string]string{}
	for _, device := range deviceRows {
		occupiedDevices[device.NodeID+"/"+device.GroupID] = device.EnvironmentID + "/" + device.AssetID
	}
	for _, n := range locked {
		if n.State != "ready" {
			continue
		}
		var info api.NodeInfo
		if err = json.Unmarshal(n.Info, &info); err != nil {
			return err
		}
		info.ExternalInterfaces = infos[n.ID].ExternalInterfaces
		info.NetworkChassis = infos[n.ID].NetworkChassis
		infos[n.ID] = info
		capacity[n.ID] = infos[n.ID].Capacity
		if len(n.CapacityOverride) > 0 {
			var override api.Resources
			if err = json.Unmarshal(n.CapacityOverride, &override); err != nil {
				return err
			}
			capacity[n.ID] = override
		}
	}
	for i := range p.Updates {
		t := &p.Updates[i]
		old := findInstance(p.Before, t.Execution.InstanceId)
		var available bool
		t.Execution.PciDevices, available = deviceBindings(t.NodeID, infos[t.NodeID].VmHardware, t.Execution.Asset.PciBinding, row.ID, t.Execution.Asset.Id, occupiedDevices)
		if !available {
			return fmt.Errorf("资产 %s 的直通设备不可用", t.Execution.Asset.Name)
		}
		delta := resources(t.Execution.Asset)
		before := resources(old.Execution.Asset)
		delta.Cpu = max(0, delta.Cpu-before.Cpu)
		delta.MemoryMiB = max(0, delta.MemoryMiB-before.MemoryMiB)
		delta.DiskGiB = max(0, delta.DiskGiB-before.DiskGiB)
		if restoring {
			delta.DiskGiB = resources(t.Execution.Asset).DiskGiB
		}
		u := add(used[t.NodeID], delta)
		if !fits(u, capacity[t.NodeID]) {
			return fmt.Errorf("资产 %s 的节点容量不足", t.Execution.Asset.Name)
		}
		if restoring && !supports(infos[t.NodeID], t.Execution.Template, t.Execution.Asset.Resources.Cpu) {
			return fmt.Errorf("资产 %s 的节点不支持恢复点中的硬件", t.Execution.Asset.Name)
		}
		pool := storage[storageKey(t.NodeID, actualPool(t.NodeID, t.Execution))]
		if !restoring {
			var ok bool
			t.Execution.VolumeSources, ok = volumeBindings(t.Execution.Asset, volumes, storage, t.NodeID)
			if !ok {
				return fmt.Errorf("资产 %s 的数据卷不在当前节点", t.Execution.Asset.Name)
			}
		}
		if pool.err != nil {
			return fmt.Errorf("资产 %s 的存储不可用：%w", t.Execution.Asset.Name, pool.err)
		}
		if !pool.ready || diskUsed[pool.filesystem()]+delta.DiskGiB > diskCapacity[pool.filesystem()] {
			return fmt.Errorf("资产 %s 的存储容量不足", t.Execution.Asset.Name)
		}
		diskUsed[pool.filesystem()] += delta.DiskGiB
		assignStorage(&t.Execution, pool)
		used[t.NodeID] = u
	}
	// Large requirements go first, then choose the lowest dominant utilization.
	sort.SliceStable(p.Targets, func(i, j int) bool {
		return p.Targets[i].Execution.Asset.Resources.MemoryMiB > p.Targets[j].Execution.Asset.Resources.MemoryMiB
	})
	for i := range p.Targets {
		t := &p.Targets[i]
		requirement := resources(t.Execution.Asset)
		best := ""
		var bestPool storageCandidate
		score := 2.0
		preferred := ""
		if t.Execution.Asset.StoragePoolId != nil {
			preferred = *t.Execution.Asset.StoragePoolId
		}
		if t.Execution.Asset.Volumes != nil {
			for _, volume := range *t.Execution.Asset.Volumes {
				if volume.PersistentVolumeId != nil {
					preferred = volumes[*volume.PersistentVolumeId].StoragePoolID
				}
			}
		}
		previousNode := ""
		diskRequirement := requirement.DiskGiB
		if t.Execution.PreviousInstanceId != nil {
			old := findInstance(p.Before, *t.Execution.PreviousInstanceId)
			preferred, previousNode = actualPool(old.NodeID, old.Execution), old.NodeID
			if t.Execution.Asset.Volumes != nil && old.Execution.Asset.Volumes != nil {
				for _, volume := range *t.Execution.Asset.Volumes {
					for _, existing := range *old.Execution.Asset.Volumes {
						if volume.Id == existing.Id && volume.PersistentVolumeId == nil && existing.PersistentVolumeId == nil {
							diskRequirement -= min(volume.SizeGiB, existing.SizeGiB)
						}
					}
				}
			}
		}
		for _, n := range locked {
			if previousNode != "" && n.ID != previousNode {
				continue
			}
			c, ok := capacity[n.ID]
			if _, ready := deviceBindings(n.ID, infos[n.ID].VmHardware, t.Execution.Asset.PciBinding, row.ID, t.Execution.Asset.Id, occupiedDevices); !ready {
				continue
			}
			if !ok || !supports(infos[n.ID], t.Execution.Template, t.Execution.Asset.Resources.Cpu) {
				continue
			}
			if !restoring {
				if _, ok := volumeBindings(t.Execution.Asset, volumes, storage, n.ID); !ok {
					continue
				}
			}
			u := add(used[n.ID], requirement)
			if !fits(u, c) {
				continue
			}
			for _, pool := range storage {
				key := pool.filesystem()
				if pool.node != n.ID || !pool.ready || pool.err != nil || preferred != "" && preferred != pool.id || diskUsed[key]+diskRequirement > diskCapacity[key] {
					continue
				}
				if pool.info.Rbd != nil && t.Execution.Template.Kind != api.Vm {
					continue
				}
				s := max(float64(u.Cpu)/float64(c.Cpu), float64(u.MemoryMiB)/float64(c.MemoryMiB), float64(diskUsed[key]+diskRequirement)/float64(diskCapacity[key]), 1-float64(pool.info.AvailableBytes)/float64(pool.info.CapacityBytes))
				if s < score || s == score && pool.id < bestPool.id {
					best, bestPool, score = n.ID, pool, s
				}
			}
		}
		if best == "" {
			failures := []string{}
			for _, pool := range storage {
				if pool.err != nil {
					failures = append(failures, pool.err.Error())
				}
			}
			return fmt.Errorf("没有节点可承载资产 %s（%d 核，%d MiB，%d GiB）%s", t.Execution.Asset.Name, requirement.Cpu, requirement.MemoryMiB, requirement.DiskGiB, strings.Join(failures, "；"))
		}
		t.NodeID = best
		t.Execution.PciDevices, _ = deviceBindings(best, infos[best].VmHardware, t.Execution.Asset.PciBinding, row.ID, t.Execution.Asset.Id, occupiedDevices)
		if binding := t.Execution.Asset.PciBinding; binding != nil {
			for _, id := range binding.GroupIds {
				occupiedDevices[best+"/"+id] = row.ID + "/" + t.Execution.Asset.Id
			}
		}
		assignStorage(&t.Execution, bestPool)
		if !restoring {
			t.Execution.VolumeSources, _ = volumeBindings(t.Execution.Asset, volumes, storage, best)
		}
		diskUsed[bestPool.filesystem()] += diskRequirement
		used[best] = add(used[best], requirement)
	}
	if restoring {
		for _, group := range [][]Target{p.Targets, p.Updates} {
			for i := range group {
				t := &group[i]
				t.Execution.VolumeSources = map[string]api.NodeVolume{}
				source := sources[t.Execution.Asset.Id]
				if source.Execution.Asset.Volumes == nil {
					continue
				}
				for _, original := range *source.Execution.Asset.Volumes {
					if original.PersistentVolumeId == nil {
						continue
					}
					pool := storage[storageKey(t.NodeID, actualPool(t.NodeID, t.Execution))]
					id := uuid.NewString()
					for j := range *t.Execution.Asset.Volumes {
						v := &(*t.Execution.Asset.Volumes)[j]
						if v.Id != original.Id {
							continue
						}
						v.PersistentVolumeId = &id
						t.Execution.VolumeSources[v.Id] = api.NodeVolume{Id: id, Kind: t.Execution.Template.Kind, SizeGiB: v.SizeGiB, Storage: pool.info}
						if err = q.CreatePersistentVolume(ctx, queries.CreatePersistentVolumeParams{ID: id, NodeID: t.NodeID, StoragePoolID: pool.id, Name: t.Execution.Asset.Name + " · " + v.MountPath, Kind: string(t.Execution.Template.Kind), SizeGib: v.SizeGiB, OperationID: &op.ID}); err != nil {
							return err
						}
					}
				}
				for j := range p.Spec.Assets {
					if p.Spec.Assets[j].Id == t.Execution.Asset.Id {
						p.Spec.Assets[j] = t.Execution.Asset
					}
				}
			}
		}
	}
	if !serviceOnly {
		raw, err := resourceRecords(row.ID, p.Targets)
		if err != nil {
			return err
		}
		if err = q.ReserveAssets(ctx, raw); err != nil {
			return err
		}
		updates := p.Updates
		if restoring {
			updates = recoveryReservations(updates, p.Before)
		}
		raw, err = resourceRecords(row.ID, updates)
		if err != nil {
			return err
		}
		if err = q.ReserveResourceUpdates(ctx, raw); err != nil {
			return err
		}
		if err = reserveExternal(ctx, q, row.ID, p.Spec, infos); err != nil {
			return err
		}
	}
	if p.Owner != nil {
		if err = q.SetNetworkOwner(ctx, queries.SetNetworkOwnerParams{ID: row.ID, NetworkNodeID: &p.Owner.NodeID}); err != nil {
			return err
		}
	}
	info := api.NodeInfo{}
	if p.Owner != nil {
		info = infos[p.Owner.NodeID]
	}
	if err = w.reserveServices(ctx, q, op, p, info); err != nil {
		return err
	}
	transactionWorker := w
	transactionWorker.Queries = q
	phase := "prepare"
	if restoring {
		phase = "prepare-recovery"
	}
	if serviceOnly {
		phase = "services"
	}
	if err = transactionWorker.phase(ctx, op, p, phase); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: %v", errPersistence, err)
	}
	return nil
}

func resources(a api.Asset) api.Resources {
	r := a.Resources
	if a.Volumes != nil {
		for _, v := range *a.Volumes {
			if v.PersistentVolumeId == nil {
				r.DiskGiB += v.SizeGiB
			}
		}
	}
	return r
}
func add(a, b api.Resources) api.Resources {
	return api.Resources{Cpu: a.Cpu + b.Cpu, MemoryMiB: a.MemoryMiB + b.MemoryMiB, DiskGiB: a.DiskGiB + b.DiskGiB}
}
func fits(a, b api.Resources) bool {
	return a.Cpu <= b.Cpu && a.MemoryMiB <= b.MemoryMiB
}
func findInstance(targets []Target, id string) Target {
	for _, t := range targets {
		if t.Execution.InstanceId == id {
			return t
		}
	}
	return Target{}
}
