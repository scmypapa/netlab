package network

import (
	"context"
	"fmt"

	"github.com/ovn-org/libovsdb/ovsdb"
)

type SFlow struct {
	UUID        string            `ovsdb:"_uuid"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}

type IPFIX struct {
	UUID               string            `ovsdb:"_uuid"`
	Targets            []string          `ovsdb:"targets"`
	Sampling           *int              `ovsdb:"sampling"`
	CacheActiveTimeout *int              `ovsdb:"cache_active_timeout"`
	CacheMaxFlows      *int              `ovsdb:"cache_max_flows"`
	OtherConfig        map[string]string `ovsdb:"other_config"`
	ExternalIDs        map[string]string `ovsdb:"external_ids"`
}

func (n *OVS) SetFlowExport(ctx context.Context, node, target string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	b := &Bridge{Name: n.bridge}
	if err := n.client.Get(ctx, b); err != nil {
		return err
	}
	if b.SFlow != nil {
		old := &SFlow{UUID: *b.SFlow}
		if err := n.client.Get(ctx, old); err != nil {
			return err
		}
		if old.ExternalIDs["netlab.node"] == node {
			b.SFlow = nil
		}
	}
	flow := &IPFIX{}
	if b.IPFIX != nil {
		flow.UUID = *b.IPFIX
		if err := n.client.Get(ctx, flow); err != nil {
			return err
		}
		if flow.ExternalIDs["netlab.node"] != node {
			return fmt.Errorf("OVS bridge %s has another IPFIX collector", n.bridge)
		}
	}
	if target == "" {
		if b.IPFIX == nil {
			return nil
		}
		b.IPFIX = nil
		ops, err := n.client.Where(b).Update(b, &b.IPFIX)
		if err != nil {
			return err
		}
		return n.applyFlowExport(ctx, ops)
	}
	rate, timeout, capacity := 1, 5, 65536
	flow.Targets, flow.Sampling, flow.CacheActiveTimeout, flow.CacheMaxFlows = []string{target}, &rate, &timeout, &capacity
	flow.OtherConfig = map[string]string{"enable-input-sampling": "true", "enable-output-sampling": "true", "enable-tunnel-sampling": "false"}
	if flow.UUID != "" {
		ops, err := n.client.Where(flow).Update(flow, &flow.Targets, &flow.Sampling, &flow.CacheActiveTimeout, &flow.CacheMaxFlows, &flow.OtherConfig)
		if err != nil {
			return err
		}
		more, err := n.client.Where(b).Update(b, &b.SFlow)
		if err != nil {
			return err
		}
		return n.applyFlowExport(ctx, append(ops, more...))
	}
	flow.UUID, flow.ExternalIDs = "new_ipfix", map[string]string{"netlab.node": node}
	ops, err := n.client.Create(flow)
	if err != nil {
		return err
	}
	b.IPFIX = &flow.UUID
	more, err := n.client.Where(b).Update(b, &b.IPFIX, &b.SFlow)
	if err != nil {
		return err
	}
	return n.applyFlowExport(ctx, append(ops, more...))
}

// OVSDB commit precedes datapath reconfiguration; acknowledge the applied generation
// before accepting traffic or replacing the collector's UDP endpoint.
func (n *OVS) applyFlowExport(ctx context.Context, ops []ovsdb.Operation) error {
	var roots []OpenVSwitch
	if err := n.client.List(ctx, &roots); err != nil {
		return err
	}
	if len(roots) != 1 {
		return fmt.Errorf("OVS root configuration is missing")
	}
	root := ovsdb.UUID{GoUUID: roots[0].UUID}
	where := []ovsdb.Condition{ovsdb.NewCondition("_uuid", ovsdb.ConditionEqual, root)}
	ops = append(ops,
		ovsdb.Operation{Op: ovsdb.OperationMutate, Table: "Open_vSwitch", Where: where, Mutations: []ovsdb.Mutation{{Column: "next_cfg", Mutator: ovsdb.MutateOperationAdd, Value: 1}}},
		ovsdb.Operation{Op: ovsdb.OperationSelect, Table: "Open_vSwitch", Where: where, Columns: []string{"next_cfg"}},
	)
	results, err := n.client.Transact(ctx, ops...)
	if err != nil {
		return err
	}
	if failures, err := ovsdb.CheckOperationResults(results, ops); err != nil {
		return fmt.Errorf("OVSDB transaction: %w (%v)", err, failures)
	}
	revision := results[len(results)-1].Rows[0]["next_cfg"]
	timeout := 5000
	return transact(ctx, n.client, []ovsdb.Operation{{Op: ovsdb.OperationWait, Table: "Open_vSwitch", Where: append(where, ovsdb.NewCondition("cur_cfg", ovsdb.ConditionGreaterThanOrEqual, revision)), Columns: []string{"_uuid"}, Until: "==", Rows: []ovsdb.Row{{"_uuid": root}}, Timeout: &timeout}})
}

// Kernel ifindex identifies the actual ingress/egress attachment, even when environments reuse IPs.
func (n *OVS) FlowInterfaces(ctx context.Context) (map[uint32]string, error) {
	b := &Bridge{Name: n.bridge}
	if err := n.client.Get(ctx, b); err != nil {
		return nil, err
	}
	var ports []Port
	var interfaces []Interface
	if err := n.client.List(ctx, &ports); err != nil {
		return nil, err
	}
	if err := n.client.List(ctx, &interfaces); err != nil {
		return nil, err
	}
	byID := map[string]Interface{}
	for _, iface := range interfaces {
		byID[iface.UUID] = iface
	}
	bridgePorts := make(map[string]bool, len(b.Ports))
	for _, id := range b.Ports {
		bridgePorts[id] = true
	}
	result := map[uint32]string{}
	for _, port := range ports {
		if !bridgePorts[port.UUID] {
			continue
		}
		for _, id := range port.Interfaces {
			iface := byID[id]
			logicalPort := iface.ExternalIDs["iface-id"]
			if logicalPort != "" && iface.IfIndex != nil && *iface.IfIndex > 0 {
				result[uint32(*iface.IfIndex)] = logicalPort
			}
		}
	}
	return result, nil
}
