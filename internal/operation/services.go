package operation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"

	"github.com/jackc/pgx/v5"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/environment"
)

func serviceBinding(spec api.EnvironmentSpec, service api.ServiceExposure, port int) api.NodeServiceBinding {
	address := ""
	for _, asset := range spec.Assets {
		if asset.Id != service.AssetId {
			continue
		}
		for _, nic := range asset.Interfaces {
			if nic.Id == service.InterfaceId {
				address = nic.Address
			}
		}
	}
	return api.NodeServiceBinding{Id: service.Id, AssetId: service.AssetId, InterfaceId: service.InterfaceId, Protocol: api.ServiceProtocol(service.Protocol), TargetAddress: address, TargetPort: service.TargetPort, ListenPort: port}
}

func (w Worker) loadServices(ctx context.Context, row queries.Environment, p *Payload) error {
	if row.GatewayAddress == nil && len(environment.Services(p.Spec)) == 0 && (p.BeforeSpec == nil || len(environment.Services(*p.BeforeSpec)) == 0) {
		return nil
	}
	if row.GatewayAddress != nil {
		p.Gateway = &api.ServiceGateway{NodeId: *row.NetworkNodeID, Address: row.GatewayAddress.String()}
	}
	ports, err := w.Queries.GetServicePorts(ctx, row.ID)
	if err != nil {
		return err
	}
	if p.BeforeSpec != nil {
		for _, service := range environment.Services(*p.BeforeSpec) {
			for _, port := range ports {
				if port.State == "applied" && port.ServiceID == service.Id && port.Protocol == string(service.Protocol) {
					p.BeforeBindings = append(p.BeforeBindings, serviceBinding(*p.BeforeSpec, service, int(*port.Port)))
				}
			}
		}
	}
	return nil
}

func (w Worker) reserveServices(ctx context.Context, q *queries.Queries, op *queries.Operation, p *Payload, info api.NodeInfo) error {
	services := environment.Services(p.Spec)
	if len(services) == 0 {
		return nil
	}
	if p.Owner == nil || info.ServiceNetwork == nil {
		return errors.New("网络节点未配置服务网关")
	}
	if p.Gateway == nil {
		prefix, err := netip.ParsePrefix(info.ServiceNetwork.Cidr)
		if err != nil {
			return fmt.Errorf("服务网关地址段：%w", err)
		}
		bridge, err := netip.ParseAddr(info.ServiceNetwork.Address)
		if err != nil {
			return err
		}
		used, err := q.ListGatewayAddresses(ctx, &p.Owner.NodeID)
		if err != nil {
			return err
		}
		occupied := map[string]bool{}
		for _, address := range used {
			occupied[address] = true
		}
		var address netip.Addr
		for candidate := prefix.Masked().Addr().Next(); prefix.Contains(candidate) && prefix.Contains(candidate.Next()); candidate = candidate.Next() {
			if candidate != bridge && !occupied[candidate.String()] {
				address = candidate
				break
			}
		}
		if !address.IsValid() {
			return errors.New("服务网关地址段已分配完")
		}
		if err = q.SetGatewayAddress(ctx, queries.SetGatewayAddressParams{EnvironmentID: *op.EnvironmentID, Address: &address}); err != nil {
			return err
		}
		p.Gateway = &api.ServiceGateway{NodeId: p.Owner.NodeID, Address: address.String()}
	}
	for _, service := range services {
		port := 0
		for _, old := range p.BeforeBindings {
			if old.Id == service.Id && old.Protocol == api.ServiceProtocol(service.Protocol) && (service.ListenPort == nil || *service.ListenPort == old.ListenPort) {
				port = old.ListenPort
				break
			}
		}
		if port == 0 {
			requested := 0
			if service.ListenPort != nil {
				requested = *service.ListenPort
			}
			reserved, err := q.ReserveServicePort(ctx, queries.ReserveServicePortParams{NodeID: p.Owner.NodeID, Protocol: string(service.Protocol), EnvironmentID: *op.EnvironmentID, ServiceID: service.Id, OperationID: op.ID, RequestedPort: int32(requested)})
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("服务 %s 的监听端口不可用", service.Id)
			}
			if err != nil {
				return err
			}
			if reserved.Port != nil {
				port = int(*reserved.Port)
			}
		}
		p.Bindings = append(p.Bindings, serviceBinding(p.Spec, service, port))
	}
	return nil
}

func (w Worker) serviceRules(ctx context.Context, op *queries.Operation, p *Payload, spec api.EnvironmentSpec, bindings []api.NodeServiceBinding) ([]api.NodeServiceBinding, error) {
	if p.Gateway == nil {
		return bindings, nil
	}
	nodes, err := w.Queries.GetNodeEndpoints(ctx, []string{p.Gateway.NodeId})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errPersistence, err)
	}
	if len(nodes) != 1 {
		return nil, errors.New("服务网关节点不存在")
	}
	if bindings == nil {
		bindings = []api.NodeServiceBinding{}
	}
	result, err := w.Client.Execute(ctx, nodes[0].Endpoint, api.NodePlan{OperationId: op.ID, EnvironmentId: *op.EnvironmentID, Phase: api.NodePlanPhaseServices, Assets: []api.AssetExecution{}, Spec: spec, Gateway: p.Gateway, Services: &bindings})
	if err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, errors.New(*result.Error)
	}
	if result.Services == nil || len(*result.Services) != len(bindings) {
		return nil, errors.New("节点未确认完整服务映射结果")
	}
	expected := map[string]api.NodeServiceBinding{}
	for _, binding := range bindings {
		expected[binding.Id] = binding
	}
	for _, resolved := range *result.Services {
		binding, exists := expected[resolved.Id]
		definition := resolved
		definition.ListenPort = binding.ListenPort
		if !exists || definition != binding || resolved.ListenPort < 1 || resolved.ListenPort > 65535 || binding.ListenPort != 0 && binding.ListenPort != resolved.ListenPort {
			return nil, errors.New("节点服务映射结果与任务不符")
		}
		delete(expected, resolved.Id)
	}
	return *result.Services, nil
}

func (w Worker) quiesceServices(ctx context.Context, op *queries.Operation, p *Payload, affected []Target) error {
	if len(p.BeforeBindings) == 0 {
		return nil
	}
	remaining := slices.Clone(p.BeforeBindings)
	removed := []string{}
	remaining = slices.DeleteFunc(remaining, func(binding api.NodeServiceBinding) bool {
		remove := !slices.ContainsFunc(p.Bindings, func(next api.NodeServiceBinding) bool { return next.Id == binding.Id }) ||
			slices.ContainsFunc(affected, func(target Target) bool { return target.Execution.Asset.Id == binding.AssetId })
		if remove {
			removed = append(removed, binding.Id)
		}
		return remove
	})
	if len(removed) == 0 {
		return nil
	}
	if _, err := w.serviceRules(ctx, op, p, *p.BeforeSpec, remaining); err != nil {
		return err
	}
	if err := w.Queries.ReserveAppliedServicePorts(ctx, queries.ReserveAppliedServicePortsParams{OperationID: op.ID, EnvironmentID: *op.EnvironmentID, ServiceIds: removed}); err != nil {
		return fmt.Errorf("%w: %v", errPersistence, err)
	}
	return nil
}

func commitServices(ctx context.Context, q *queries.Queries, environmentID string, bindings []api.NodeServiceBinding) error {
	if bindings == nil {
		bindings = []api.NodeServiceBinding{}
	}
	raw, err := json.Marshal(bindings)
	if err != nil {
		return err
	}
	if err = q.DeleteUnusedServicePorts(ctx, queries.DeleteUnusedServicePortsParams{EnvironmentID: environmentID, Bindings: raw}); err != nil {
		return err
	}
	return q.ApplyServicePorts(ctx, queries.ApplyServicePortsParams{EnvironmentID: environmentID, Bindings: raw})
}
