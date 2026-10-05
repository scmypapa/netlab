package network

import (
	"context"
	"fmt"
	"slices"
)

// Capture devices follow the same logical port identity as VM taps and container veths.
func (n *OVS) CaptureDevices(ctx context.Context, logicalPorts []string) ([]string, error) {
	bridge := &Bridge{Name: n.bridge}
	if err := n.client.Get(ctx, bridge); err != nil {
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
	attached := map[string]bool{}
	for _, port := range ports {
		if slices.Contains(bridge.Ports, port.UUID) {
			for _, id := range port.Interfaces {
				attached[id] = true
			}
		}
	}
	devices := map[string]string{}
	for _, iface := range interfaces {
		if attached[iface.UUID] {
			devices[iface.ExternalIDs["iface-id"]] = iface.Name
		}
	}
	result := make([]string, 0, len(logicalPorts))
	for _, port := range logicalPorts {
		device := devices[port]
		if device == "" {
			return nil, fmt.Errorf("抓包接口未接入运行节点：%s", port)
		}
		result = append(result, device)
	}
	return result, nil
}
