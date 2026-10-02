package operation

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"

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
		r := t.Execution.Asset.Resources
		if t.Execution.Asset.Volumes != nil {
			for _, v := range *t.Execution.Asset.Volumes {
				r.DiskGiB += v.SizeGiB
			}
		}
		rows = append(rows, reservation{id, t.Execution.Asset.Id, t.Execution.InstanceId, t.NodeID, t.Execution, r})
	}
	return json.Marshal(rows)
}

func (w Worker) plan(ctx context.Context, op *queries.Operation, p *Payload) error {
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
	for _, n := range nodes {
		byNode[n.ID] = n
		var info api.NodeInfo
		if err = json.Unmarshal(n.Info, &info); err != nil {
			return err
		}
		infos[n.ID] = info
	}
	if len(row.AppliedSpec) > 0 {
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
	if op.Kind != "change" && op.Kind != "rebuild" && op.Kind != "start" && op.Kind != "destroy" {
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
		templates, err := environment.Templates(ctx, w.Queries, p.Spec.Assets)
		if err != nil {
			return err
		}
		for _, a := range p.Spec.Assets {
			t, ok := templates[a.TemplateId]
			if !ok || t.State == nil || *t.State != api.Ready {
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
	capacity, used := map[string]api.Resources{}, map[string]api.Resources{}
	reservations, err := q.GetNodeReservations(ctx, ids)
	if err != nil {
		return err
	}
	for _, r := range reservations {
		used[r.NodeID] = api.Resources{Cpu: int(r.Cpu), MemoryMiB: r.MemoryMib, DiskGiB: r.DiskGib}
	}
	for _, n := range locked {
		if n.State != "ready" {
			continue
		}
		var info api.NodeInfo
		if err = json.Unmarshal(n.Info, &info); err != nil {
			return err
		}
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
	for _, t := range p.Updates {
		old := findInstance(p.Before, t.Execution.InstanceId)
		delta := resources(t.Execution.Asset)
		before := resources(old.Execution.Asset)
		delta.Cpu = max(0, delta.Cpu-before.Cpu)
		delta.MemoryMiB = max(0, delta.MemoryMiB-before.MemoryMiB)
		delta.DiskGiB = max(0, delta.DiskGiB-before.DiskGiB)
		u := add(used[t.NodeID], delta)
		if !fits(u, capacity[t.NodeID]) {
			return fmt.Errorf("资产 %s 的节点容量不足", t.Execution.Asset.Name)
		}
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
		score := 2.0
		for _, n := range locked {
			c, ok := capacity[n.ID]
			if !ok || !supports(infos[n.ID], t.Execution.Template, t.Execution.Asset.Resources.Cpu) {
				continue
			}
			u := add(used[n.ID], requirement)
			if !fits(u, c) {
				continue
			}
			s := max(float64(u.Cpu)/float64(c.Cpu), float64(u.MemoryMiB)/float64(c.MemoryMiB), float64(u.DiskGiB)/float64(c.DiskGiB))
			if s < score {
				best, score = n.ID, s
			}
		}
		if best == "" {
			return fmt.Errorf("没有节点可承载资产 %s（%d 核，%d MiB，%d GiB）", t.Execution.Asset.Name, requirement.Cpu, requirement.MemoryMiB, requirement.DiskGiB)
		}
		// Replacement keeps shared local volumes on their owning node.
		if t.Execution.PreviousInstanceId != nil && t.Execution.Asset.Volumes != nil && len(*t.Execution.Asset.Volumes) > 0 {
			old := findInstance(p.Before, *t.Execution.PreviousInstanceId)
			c, ok := capacity[old.NodeID]
			if !ok || !fits(add(used[old.NodeID], requirement), c) || !supports(infos[old.NodeID], t.Execution.Template, t.Execution.Asset.Resources.Cpu) {
				return fmt.Errorf("资产 %s 的数据卷所在节点容量不足", t.Execution.Asset.Name)
			}
			best = old.NodeID
		}
		t.NodeID = best
		used[best] = add(used[best], requirement)
	}
	if !serviceOnly {
		raw, err := resourceRecords(row.ID, p.Targets)
		if err != nil {
			return err
		}
		if err = q.ReserveAssets(ctx, raw); err != nil {
			return err
		}
		raw, err = resourceRecords(row.ID, p.Updates)
		if err != nil {
			return err
		}
		if err = q.ReserveResourceUpdates(ctx, raw); err != nil {
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
			r.DiskGiB += v.SizeGiB
		}
	}
	return r
}
func add(a, b api.Resources) api.Resources {
	return api.Resources{Cpu: a.Cpu + b.Cpu, MemoryMiB: a.MemoryMiB + b.MemoryMiB, DiskGiB: a.DiskGiB + b.DiskGiB}
}
func fits(a, b api.Resources) bool {
	return a.Cpu <= b.Cpu && a.MemoryMiB <= b.MemoryMiB && a.DiskGiB <= b.DiskGiB
}
func findInstance(targets []Target, id string) Target {
	for _, t := range targets {
		if t.Execution.InstanceId == id {
			return t
		}
	}
	return Target{}
}
