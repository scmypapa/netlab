package network

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"netlab.local/core/api"
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
		created, err := n.client.Create(iface, port, bridge)
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

// The provider is a separate logical switch. Guest addresses and interfaces stay unchanged.
func gatewayModels(plan api.NodePlan, router *Router, prefix netip.Prefix, chassis string) ([]model.Model, error) {
	if plan.Gateway == nil {
		return nil, nil
	}
	address, err := netip.ParseAddr(plan.Gateway.Address)
	if err != nil || !prefix.IsValid() || !prefix.Contains(address) || address == prefix.Addr().Next() {
		return nil, fmt.Errorf("environment gateway is outside the node service address pool")
	}
	for _, network := range plan.Spec.Networks {
		guest, err := netip.ParsePrefix(network.Cidr)
		if err != nil {
			return nil, err
		}
		if prefix.Overlaps(guest) {
			return nil, fmt.Errorf("network %s overlaps reserved service network %s", network.Name, prefix)
		}
	}
	owner := func() map[string]string {
		ids := ownership(plan.EnvironmentId)
		ids["netlab.component"] = "service_gateway"
		return ids
	}
	switchName := objectName("access", plan.EnvironmentId, "provider")
	routerPort := &RouterPort{UUID: "access_router_port", Name: objectName("access_rp", plan.EnvironmentId, "gateway"), MAC: routerMAC(address), Networks: []string{netip.PrefixFrom(address, prefix.Bits()).String()}, ExternalIDs: owner()}
	routerSwitchPort := &SwitchPort{UUID: "access_router_switch_port", Name: objectName("access_sp", plan.EnvironmentId, "gateway"), Type: "router", Addresses: []string{"router"}, Options: map[string]string{"router-port": routerPort.Name}, ExternalIDs: owner()}
	localPort := &SwitchPort{UUID: "access_localnet", Name: objectName("access_localnet", plan.EnvironmentId, "provider"), Type: "localnet", Addresses: []string{"unknown"}, Options: map[string]string{"network_name": providerNetwork(plan.Gateway.NodeId)}, ExternalIDs: owner()}
	switchModel := &Switch{UUID: "access_switch", Name: switchName, Ports: []string{routerSwitchPort.UUID, localPort.UUID}, ExternalIDs: owner()}
	router.Ports = append(router.Ports, routerPort.UUID)
	if router.Options == nil {
		router.Options = map[string]string{}
	}
	// LB return SNAT requires a gateway router; L2 stays distributed across workers.
	router.Options["chassis"] = chassis
	router.Options["lb_force_snat_ip"] = "router_ip"
	models := []model.Model{switchModel, routerPort, routerSwitchPort, localPort}
	if plan.Services != nil {
		for index, service := range *plan.Services {
			if service.ListenPort == 0 {
				continue
			}
			protocol := string(service.Protocol)
			lb := &LoadBalancer{UUID: "access_lb" + strconv.Itoa(index), Name: objectName("access_lb", plan.EnvironmentId, service.Id), Protocol: &protocol, VIPs: map[string]string{net.JoinHostPort(address.String(), strconv.Itoa(service.ListenPort)): net.JoinHostPort(service.TargetAddress, strconv.Itoa(service.TargetPort))}, ExternalIDs: owner()}
			lb.ExternalIDs["netlab.service"] = service.Id
			lb.ExternalIDs["netlab.asset"] = service.AssetId
			lb.ExternalIDs["netlab.interface"] = service.InterfaceId
			router.LoadBalancers = append(router.LoadBalancers, lb.UUID)
			models = append(models, lb)
		}
	}
	return models, nil
}

func (n *OVN) ApplyServices(ctx context.Context, plan api.NodePlan) error {
	router := &Router{Name: objectName("lr", plan.EnvironmentId, "gateway")}
	var routers []Router
	if err := n.client.Where(router).List(ctx, &routers); err != nil {
		return fmt.Errorf("environment router: %w", err)
	}
	if len(routers) == 0 && (plan.Services == nil || len(*plan.Services) == 0) {
		ops, err := n.gatewayDeleteOperations(plan.EnvironmentId)
		if err != nil {
			return err
		}
		return transact(ctx, n.client, ops)
	}
	if len(routers) != 1 {
		return fmt.Errorf("environment router %s: expected one match, found %d", router.Name, len(routers))
	}
	router = &routers[0]
	port := &RouterPort{Name: objectName("access_rp", plan.EnvironmentId, "gateway")}
	if err := n.client.Get(ctx, port); err == nil {
		for index, id := range router.Ports {
			if id == port.UUID {
				router.Ports = append(router.Ports[:index], router.Ports[index+1:]...)
				break
			}
		}
	} else if err != client.ErrNotFound {
		return err
	}
	router.LoadBalancers = nil
	models, err := gatewayModels(plan, router, n.provider, n.chassis)
	if err != nil {
		return err
	}
	ops, err := n.gatewayDeleteOperations(plan.EnvironmentId)
	if err != nil {
		return err
	}
	if len(models) > 0 {
		create, err := n.client.Create(models...)
		if err != nil {
			return err
		}
		ops = append(ops, create...)
	}
	update, err := n.client.Where(router).Update(router, &router.Ports, &router.LoadBalancers, &router.Options)
	if err != nil {
		return err
	}
	return transact(ctx, n.client, append(ops, update...))
}

func (n *OVN) gatewayDeleteOperations(environmentID string) ([]ovsdb.Operation, error) {
	ids := ownership(environmentID)
	ids["netlab.component"] = "service_gateway"
	value, err := ovsdb.NewOvsMap(ids)
	if err != nil {
		return nil, err
	}
	var ops []ovsdb.Operation
	for _, table := range []string{"Logical_Switch", "Logical_Switch_Port", "Logical_Router_Port", "Load_Balancer"} {
		ops = append(ops, ovsdb.Operation{Op: ovsdb.OperationDelete, Table: table, Where: []ovsdb.Condition{{Column: "external_ids", Function: ovsdb.ConditionIncludes, Value: value}}})
	}
	return ops, nil
}
