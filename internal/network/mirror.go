package network

import (
	"context"
	"fmt"
	"slices"

	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
)

// Mirror ports are selected by OVN logical interface identity, shared by VM taps and container veths.
func (n *OVS) Mirror(ctx context.Context, id, environment, device string, logicalPorts []string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	bridge := &Bridge{Name: n.bridge}
	if err := n.client.Get(ctx, bridge); err != nil {
		return err
	}
	var ports []Port
	var interfaces []Interface
	if err := n.client.List(ctx, &ports); err != nil {
		return err
	}
	if err := n.client.List(ctx, &interfaces); err != nil {
		return err
	}
	byID := map[string]string{}
	for _, iface := range interfaces {
		byID[iface.UUID] = iface.ExternalIDs["iface-id"]
	}
	selected := []string{}
	for _, port := range ports {
		if !slices.Contains(bridge.Ports, port.UUID) {
			continue
		}
		for _, iface := range port.Interfaces {
			if slices.Contains(logicalPorts, byID[iface]) {
				selected = append(selected, port.UUID)
				break
			}
		}
	}
	if len(selected) != len(logicalPorts) {
		return fmt.Errorf("抓包接口未全部接入运行节点：%d/%d", len(selected), len(logicalPorts))
	}
	ids := map[string]string{"netlab.capture": id, "netlab.environment": environment}
	iface := &Interface{UUID: "capture_iface", Name: device, ExternalIDs: ids}
	port := &Port{UUID: "capture_port", Name: device, Interfaces: []string{iface.UUID}, ExternalIDs: ids}
	mirror := &Mirror{UUID: "capture_mirror", Name: "netlab.capture." + id, SourcePorts: selected, DestinationPorts: selected, OutputPort: &port.UUID, ExternalIDs: ids}
	ops, err := n.client.Create(iface, port, mirror)
	if err != nil {
		return err
	}
	more, err := n.client.Where(bridge).Mutate(bridge,
		model.Mutation{Field: &bridge.Ports, Mutator: ovsdb.MutateOperationInsert, Value: []string{port.UUID}},
		model.Mutation{Field: &bridge.Mirrors, Mutator: ovsdb.MutateOperationInsert, Value: []string{mirror.UUID}})
	if err != nil {
		return err
	}
	return transact(ctx, n.client, append(ops, more...))
}

func (n *OVS) RemoveMirror(ctx context.Context, id string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	var mirrors []Mirror
	if err := n.client.WhereCache(func(m *Mirror) bool { return m.ExternalIDs["netlab.capture"] == id }).List(ctx, &mirrors); err != nil {
		return err
	}
	if len(mirrors) == 0 {
		return nil
	}
	mirror := &mirrors[0]
	bridge := &Bridge{Name: n.bridge}
	ops, err := n.client.Where(bridge).Mutate(bridge,
		model.Mutation{Field: &bridge.Mirrors, Mutator: ovsdb.MutateOperationDelete, Value: []string{mirror.UUID}})
	if err != nil {
		return err
	}
	more, err := n.client.Where(mirror).Delete()
	if err != nil {
		return err
	}
	ops = append(ops, more...)
	if mirror.OutputPort != nil {
		port := &Port{UUID: *mirror.OutputPort}
		more, err = n.client.Where(bridge).Mutate(bridge, model.Mutation{Field: &bridge.Ports, Mutator: ovsdb.MutateOperationDelete, Value: []string{port.UUID}})
		if err != nil {
			return err
		}
		ops = append(ops, more...)
		more, err = n.client.Where(port).Delete()
		if err != nil {
			return err
		}
		ops = append(ops, more...)
	}
	return transact(ctx, n.client, ops)
}
