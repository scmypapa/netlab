//go:build linux

package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"netlab.local/core/api"
)

type gatewayRecord struct {
	Gateway  api.ServiceGateway       `json:"gateway"`
	Services []api.NodeServiceBinding `json:"services"`
}

type portReservation struct {
	environment, service string
	fd                   int
}

type Gateway struct {
	mu        sync.Mutex
	directory string
	table     string
	ovn       *OVN
	prefix    netip.Prefix
	address   netip.Addr
	records   map[string]gatewayRecord
	sockets   map[servicePort]portReservation
}

func NewGateway(ctx context.Context, directory, nodeID, cidr string, ovs *OVS, ovn *OVN) (_ *Gateway, err error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() > 30 {
		return nil, fmt.Errorf("service provider requires an IPv4 prefix with gateway and environment addresses")
	}
	prefix = prefix.Masked()
	chassis, err := configureProvider(ctx, ovs, prefix, nodeID)
	if err != nil {
		return nil, err
	}
	ovn.ConfigureGateway(prefix, chassis)
	gateway := &Gateway{directory: filepath.Join(directory, "services"), table: "netlab_services_" + strings.ReplaceAll(nodeID, "-", ""), ovn: ovn, prefix: prefix, address: prefix.Addr().Next(), records: map[string]gatewayRecord{}, sockets: map[servicePort]portReservation{}}
	defer func() {
		if err != nil {
			gateway.Close()
		}
	}()
	if err = os.MkdirAll(gateway.directory, 0700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(gateway.directory)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		value, err := os.ReadFile(filepath.Join(gateway.directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		var record gatewayRecord
		if err = json.Unmarshal(value, &record); err != nil {
			return nil, err
		}
		address, err := netip.ParseAddr(record.Gateway.Address)
		if err != nil || !prefix.Contains(address) || record.Gateway.NodeId != nodeID {
			return nil, fmt.Errorf("stored service gateway does not belong to this node provider")
		}
		environment := entry.Name()[:len(entry.Name())-5]
		gateway.records[environment] = record
	}
	for environment, record := range gateway.records {
		for _, service := range record.Services {
			port, err := bindingPort(service)
			if err != nil {
				return nil, err
			}
			fd, actual, err := reservePort(port)
			if err != nil {
				withdrawn := maps.Clone(gateway.records)
				for id, previous := range withdrawn {
					previous.Services = nil
					withdrawn[id] = previous
				}
				cleanup := gateway.applyKernel(withdrawn)
				for _, previous := range gateway.records {
					cleanup = errors.Join(cleanup, clearBindingConnections(netip.MustParseAddr(previous.Gateway.Address), changedPorts(previous, gatewayRecord{})))
				}
				return nil, errors.Join(fmt.Errorf("restore service %s: %w", service.Id, err), cleanup)
			}
			gateway.sockets[actual] = portReservation{environment: environment, service: service.Id, fd: fd}
		}
	}
	if err = gateway.applyKernel(gateway.records); err != nil {
		return nil, err
	}
	return gateway, nil
}

func (g *Gateway) Network() api.ServiceNetwork {
	return api.ServiceNetwork{Cidr: g.prefix.String(), Address: g.address.String()}
}

func (g *Gateway) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for key, socket := range g.sockets {
		unix.Close(socket.fd)
		delete(g.sockets, key)
	}
}

func kernelRecords(records map[string]gatewayRecord) []kernelBinding {
	var bindings []kernelBinding
	for _, record := range records {
		address := netip.MustParseAddr(record.Gateway.Address)
		for _, service := range record.Services {
			port, _ := bindingPort(service)
			bindings = append(bindings, kernelBinding{port: port, address: address})
		}
	}
	return bindings
}

func (g *Gateway) applyKernel(records map[string]gatewayRecord) error {
	var gateways []netip.Addr
	for _, record := range records {
		gateways = append(gateways, netip.MustParseAddr(record.Gateway.Address))
	}
	return applyKernelBindings(g.table, g.address, kernelRecords(records), gateways)
}

func changedPorts(previous, next gatewayRecord) []servicePort {
	byPort := map[servicePort]api.NodeServiceBinding{}
	for _, service := range next.Services {
		port, _ := bindingPort(service)
		byPort[port] = service
	}
	var changed []servicePort
	for _, service := range previous.Services {
		port, _ := bindingPort(service)
		if byPort[port] != service || previous.Gateway.Address != next.Gateway.Address {
			changed = append(changed, port)
		}
	}
	return changed
}

func (g *Gateway) Apply(ctx context.Context, plan api.NodePlan) ([]api.NodeServiceBinding, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if plan.Gateway == nil || plan.Services == nil {
		return nil, fmt.Errorf("service application requires a gateway and the complete binding set")
	}
	previous := g.records[plan.EnvironmentId]
	byID := map[string]api.NodeServiceBinding{}
	for _, service := range previous.Services {
		byID[service.Id] = service
	}
	services := slices.Clone(*plan.Services)
	created := map[servicePort]portReservation{}
	committed := false
	defer func() {
		if !committed {
			for _, socket := range created {
				unix.Close(socket.fd)
			}
		}
	}()
	used := map[servicePort]bool{}
	for index := range services {
		service := &services[index]
		if old, exists := byID[service.Id]; service.ListenPort == 0 && exists && old.Protocol == service.Protocol {
			service.ListenPort = old.ListenPort
		}
		port, err := bindingPort(*service)
		if err != nil {
			return nil, err
		}
		if socket, exists := g.sockets[port]; exists {
			if socket.environment != plan.EnvironmentId || socket.service != service.Id {
				return nil, fmt.Errorf("service %s conflicts with a reserved host port", service.Id)
			}
		} else {
			fd, actual, err := reservePort(port)
			if err != nil {
				return nil, err
			}
			port = actual
			created[port] = portReservation{environment: plan.EnvironmentId, service: service.Id, fd: fd}
			service.ListenPort = int(port.port)
		}
		if used[port] {
			return nil, fmt.Errorf("multiple services use the same host port")
		}
		used[port] = true
	}
	plan.Services = &services
	if err := g.ovn.ApplyServices(ctx, plan); err != nil {
		return nil, err
	}
	next := gatewayRecord{Gateway: *plan.Gateway, Services: services}
	records := maps.Clone(g.records)
	records[plan.EnvironmentId] = next
	rollback := func(cause error) error {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		old := previous.Services
		oldPlan := api.NodePlan{EnvironmentId: plan.EnvironmentId, Services: &old}
		if previous.Gateway.Address != "" {
			oldPlan.Gateway = &previous.Gateway
		}
		return errors.Join(cause, g.ovn.ApplyServices(cleanup, oldPlan), g.applyKernel(g.records))
	}
	if err := g.applyKernel(records); err != nil {
		return nil, rollback(err)
	}
	if ports := changedPorts(previous, next); len(ports) > 0 {
		if err := clearBindingConnections(netip.MustParseAddr(previous.Gateway.Address), ports); err != nil {
			return nil, rollback(err)
		}
	}
	if err := g.writeRecord(plan.EnvironmentId, next); err != nil {
		return nil, rollback(err)
	}
	g.records = records
	for port, socket := range g.sockets {
		if socket.environment == plan.EnvironmentId && !used[port] {
			unix.Close(socket.fd)
			delete(g.sockets, port)
		}
	}
	for port, socket := range created {
		g.sockets[port] = socket
	}
	committed = true
	return services, nil
}

func (g *Gateway) Remove(ctx context.Context, environment string, topology bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	previous, exists := g.records[environment]
	if !exists {
		if topology {
			return g.ovn.Remove(ctx, environment)
		}
		return nil
	}
	records := maps.Clone(g.records)
	delete(records, environment)
	draining := maps.Clone(g.records)
	old := previous
	old.Services = nil
	draining[environment] = old
	if err := g.applyKernel(draining); err != nil {
		return err
	}
	if err := clearBindingConnections(netip.MustParseAddr(previous.Gateway.Address), changedPorts(previous, gatewayRecord{})); err != nil {
		return errors.Join(err, g.applyKernel(g.records))
	}
	if topology {
		if err := g.ovn.Remove(ctx, environment); err != nil {
			return errors.Join(err, g.applyKernel(g.records))
		}
	}
	if err := g.applyKernel(records); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(g.directory, environment+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	g.records = records
	for port, socket := range g.sockets {
		if socket.environment == environment {
			unix.Close(socket.fd)
			delete(g.sockets, port)
		}
	}
	return nil
}

func (g *Gateway) writeRecord(environment string, record gatewayRecord) error {
	file, err := os.CreateTemp(g.directory, ".service-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = json.NewEncoder(file).Encode(record); err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(g.directory, environment+".json"))
}
