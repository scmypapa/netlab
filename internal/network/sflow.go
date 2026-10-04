package network

import (
	"context"
	"fmt"
	"slices"
)

type SFlow struct {
	UUID        string            `ovsdb:"_uuid"`
	Targets     []string          `ovsdb:"targets"`
	Sampling    *int              `ovsdb:"sampling"`
	Polling     *int              `ovsdb:"polling"`
	Header      *int              `ovsdb:"header"`
	Agent       *string           `ovsdb:"agent"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}

func (n *OVS) SetSampling(ctx context.Context, node, target string, rate int) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	b := &Bridge{Name: n.bridge}
	if err := n.client.Get(ctx, b); err != nil {
		return err
	}
	flow := &SFlow{}
	if b.SFlow != nil {
		flow.UUID = *b.SFlow
		if err := n.client.Get(ctx, flow); err != nil {
			return err
		}
		if flow.ExternalIDs["netlab.node"] != node {
			return fmt.Errorf("OVS bridge %s has another sFlow collector", n.bridge)
		}
	}
	if target == "" {
		if b.SFlow == nil {
			return nil
		}
		b.SFlow = nil
		ops, err := n.client.Where(b).Update(b, &b.SFlow)
		if err != nil {
			return err
		}
		return transact(ctx, n.client, ops)
	}
	header, polling, agent := 128, 0, "lo"
	flow.Targets, flow.Sampling, flow.Header, flow.Polling, flow.Agent = []string{target}, &rate, &header, &polling, &agent
	if flow.UUID != "" {
		ops, err := n.client.Where(flow).Update(flow, &flow.Targets, &flow.Sampling, &flow.Header, &flow.Polling, &flow.Agent)
		if err != nil {
			return err
		}
		return transact(ctx, n.client, ops)
	}
	flow.UUID, flow.ExternalIDs = "new_sflow", map[string]string{"netlab.node": node}
	ops, err := n.client.Create(flow)
	if err != nil {
		return err
	}
	b.SFlow = &flow.UUID
	more, err := n.client.Where(b).Update(b, &b.SFlow)
	if err != nil {
		return err
	}
	return transact(ctx, n.client, append(ops, more...))
}

// Kernel ifindex identifies the actual ingress/egress attachment, even when environments reuse IPs.
func (n *OVS) SamplingInterfaces(ctx context.Context) (map[uint32]string, error) {
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
	result := map[uint32]string{}
	for _, port := range ports {
		if !slices.Contains(b.Ports, port.UUID) {
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
