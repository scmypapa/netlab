//go:build linux

package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/namespaces"
	"libvirt.org/go/libvirtxml"
	"netlab.local/core/api"
	"netlab.local/core/internal/network"
)

func policiesByNetwork(spec api.EnvironmentSpec) map[string][]api.Policy {
	result := make(map[string][]api.Policy)
	if spec.Policies != nil {
		for _, p := range *spec.Policies {
			if p.Action == api.Shape {
				result[p.NetworkId] = append(result[p.NetworkId], p)
			}
		}
	}
	return result
}
func (e *Engine) shape(ctx context.Context, env string, a api.AssetExecution, policies map[string][]api.Policy) error {
	devices, err := e.devices(ctx, env, a)
	if err != nil {
		return err
	}
	for _, i := range a.Interfaces {
		if name := devices[i.PortName]; name != "" {
			if err = network.Shape(name, i.PortName, policies[i.NetworkId]); err != nil {
				return fmt.Errorf("interface %s: %w", i.Id, err)
			}
		}
	}
	return nil
}
func (e *Engine) devices(ctx context.Context, env string, a api.AssetExecution) (map[string]string, error) {
	devices := make(map[string]string)
	if a.Template.Kind == api.Container && e.container != nil {
		ctx = namespaces.WithNamespace(ctx, "netlab")
		c, err := e.container.client.LoadContainer(ctx, a.InstanceId)
		if errdefs.IsNotFound(err) {
			return devices, nil
		}
		if err != nil {
			return nil, err
		}
		labels, err := c.Labels(ctx)
		if err != nil {
			return nil, err
		}
		if labels[environmentLabel] != env || labels[assetLabel] != a.Asset.Id {
			return nil, errors.New("container ownership does not match plan")
		}
		t, err := c.Task(ctx, nil)
		if errdefs.IsNotFound(err) {
			return devices, nil
		}
		if err != nil {
			return nil, err
		}
		state, err := t.Status(ctx)
		if err != nil {
			return nil, err
		}
		if state.Status != containerd.Stopped {
			for _, i := range a.Interfaces {
				devices[i.PortName] = deviceName(i.PortName)
			}
		}
	}
	if a.Template.Kind == api.Vm && e.vm != nil {
		d, err := e.vm.conn.LookupDomainByUUIDString(a.InstanceId)
		if noDomain(err) {
			return devices, nil
		}
		if err != nil {
			return nil, err
		}
		defer d.Free()
		if _, err = e.vm.owned(d, env, a.Asset.Id); err != nil {
			return nil, err
		}
		text, err := d.GetXMLDesc(0)
		if err != nil {
			return nil, err
		}
		var config libvirtxml.Domain
		if err = config.Unmarshal(text); err != nil {
			return nil, err
		}
		for _, i := range config.Devices.Interfaces {
			if i.Target != nil && i.VirtualPort != nil && i.VirtualPort.Params != nil && i.VirtualPort.Params.OpenVSwitch != nil {
				devices[i.VirtualPort.Params.OpenVSwitch.InterfaceID] = i.Target.Dev
			}
		}
	}
	return devices, nil
}
