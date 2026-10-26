package network

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
)

const ProviderBridge = "br-nl-service"

func providerNetwork(nodeID string) string {
	return "netlab-service-" + strings.ReplaceAll(nodeID, "-", "")
}

func (n *OVS) EnsureProvider(ctx context.Context, nodeID string) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var roots []OpenVSwitch
	if err := n.client.List(ctx, &roots); err != nil {
		return "", err
	}
	if len(roots) != 1 || roots[0].ExternalIDs["system-id"] == "" {
		return "", fmt.Errorf("OVS has no chassis system-id")
	}
	root := &roots[0]
	var ops []ovsdb.Operation
	bridge := &Bridge{Name: ProviderBridge}
	if err := n.client.Get(ctx, bridge); err == client.ErrNotFound {
		iface := &Interface{UUID: "provider_interface", Name: ProviderBridge, Type: "internal"}
		port := &Port{UUID: "provider_port", Name: ProviderBridge, Interfaces: []string{iface.UUID}}
		bridge = &Bridge{UUID: "provider_bridge", Name: ProviderBridge, Ports: []string{port.UUID}, ExternalIDs: map[string]string{"netlab.component": "service_provider"}}
		created, err := createOperations(n.client, iface, port, bridge)
		if err != nil {
			return "", err
		}
		ops = append(ops, created...)
		root.Bridges = append(root.Bridges, bridge.UUID)
	} else if err != nil {
		return "", err
	} else if bridge.ExternalIDs["netlab.component"] != "service_provider" {
		return "", fmt.Errorf("OVS bridge %s is not managed by Netlab", ProviderBridge)
	}
	mappings := strings.Split(root.ExternalIDs["ovn-bridge-mappings"], ",")
	network := providerNetwork(nodeID)
	found := false
	for _, mapping := range mappings {
		parts := strings.SplitN(mapping, ":", 2)
		if parts[0] == network {
			if len(parts) != 2 || parts[1] != ProviderBridge {
				return "", fmt.Errorf("OVN provider network %s has another bridge mapping", network)
			}
			found = true
		}
	}
	if !found {
		if root.ExternalIDs["ovn-bridge-mappings"] != "" {
			root.ExternalIDs["ovn-bridge-mappings"] += ","
		}
		root.ExternalIDs["ovn-bridge-mappings"] += network + ":" + ProviderBridge
	}
	updated, err := n.client.Where(root).Update(root, &root.Bridges, &root.ExternalIDs)
	if err != nil {
		return "", err
	}
	if err = transact(ctx, n.client, append(ops, updated...)); err != nil {
		return "", err
	}
	return root.ExternalIDs["system-id"], nil
}

func (n *OVN) ConfigureGateway(prefix netip.Prefix, chassis string) {
	n.provider, n.chassis = prefix, chassis
}

func (n *OVN) Chassis() string { return n.chassis }
