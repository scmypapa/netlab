//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/oci"
	"libvirt.org/go/libvirt"
	"libvirt.org/go/libvirtxml"
	"netlab.local/core/api"
)

func (c *Containers) update(ctx context.Context, container containerd.Container, env string, a api.AssetExecution) (string, error) {
	spec, err := container.Spec(ctx)
	if err != nil {
		return "unknown", err
	}
	info, err := container.Info(ctx)
	if err != nil {
		return "unknown", err
	}
	oldEnvironment := append([]string(nil), spec.Process.Env...)
	var oldInterfaces []api.ResolvedInterface
	if err = json.Unmarshal([]byte(info.Labels[networkLabel]), &oldInterfaces); err != nil {
		return "unknown", err
	}
	networkChanged := !reflect.DeepEqual(oldInterfaces, a.Interfaces)
	if err = oci.ApplyOpts(ctx, c.client, &info, spec, oci.WithMemoryLimit(uint64(a.Asset.Resources.MemoryMiB)<<20), cpuLimit(a.Asset.Resources.Cpu)); err != nil {
		return "unknown", err
	}
	spec.Hostname = a.Asset.Name
	image, err := container.Image(ctx)
	if err != nil {
		return "unknown", err
	}
	config, err := imageConfig(ctx, image)
	if err != nil {
		return "unknown", err
	}
	volumes := managedVolumes(config, a.Asset.Volumes)
	var oldVolumes []api.Volume
	if err = json.Unmarshal([]byte(info.Labels["netlab.volumes"]), &oldVolumes); err != nil {
		return "unknown", err
	}
	oldPaths := make(map[string]bool, len(oldVolumes))
	for _, volume := range oldVolumes {
		oldPaths[volume.MountPath] = true
	}
	mounts := spec.Mounts[:0:0]
	for _, mount := range spec.Mounts {
		if !oldPaths[mount.Destination] {
			mounts = append(mounts, mount)
		}
	}
	volumeMounts, err := c.volumeMounts(env, a.Asset.Id, volumes)
	if err != nil {
		return "unknown", err
	}
	mounts = append(mounts, volumeMounts...)
	mountsChanged := !reflect.DeepEqual(spec.Mounts, mounts)
	spec.Process.Env = nil
	if err = oci.ApplyOpts(ctx, c.client, &info, spec, oci.WithImageConfig(image)); err != nil {
		return "unknown", err
	}
	if a.Asset.Parameters != nil {
		envs := []string{}
		for k, v := range *a.Asset.Parameters {
			envs = append(envs, k+"="+v)
		}
		sort.Strings(envs)
		if err = oci.ApplyOpts(ctx, c.client, &info, spec, oci.WithEnv(envs)); err != nil {
			return "unknown", err
		}
	}
	state, err := containerState(ctx, container)
	if err != nil {
		return state, err
	}
	if state == "running" || state == "suspended" {
		if mountsChanged || networkChanged || !reflect.DeepEqual(oldEnvironment, spec.Process.Env) {
			return state, fmt.Errorf("container must be stopped before updating interfaces, volume mounts or startup parameters")
		}
		task, err := container.Task(ctx, nil)
		if err != nil {
			return state, err
		}
		if err = task.Update(ctx, containerd.WithResources(spec.Linux.Resources)); err != nil {
			return state, err
		}
	}
	if mountsChanged {
		if err = c.initializeVolumes(ctx, container, env, a.Asset.Id, volumes); err != nil {
			return state, err
		}
	}
	spec.Mounts = mounts
	if networkChanged {
		if err = writeNetworkFiles(instanceDir(c.data, env, a.InstanceId), a.Interfaces); err != nil {
			return state, err
		}
	}
	interfaces, err := json.Marshal(a.Interfaces)
	if err != nil {
		return state, err
	}
	info.Labels[networkLabel] = string(interfaces)
	volumeJSON, err := json.Marshal(volumes)
	if err != nil {
		return state, err
	}
	info.Labels["netlab.volumes"] = string(volumeJSON)
	executionJSON, err := json.Marshal(a)
	if err != nil {
		return state, err
	}
	info.Labels[executionLabel] = string(executionJSON)
	if err = container.Update(ctx, containerd.UpdateContainerOpts(containerd.WithSpec(spec)), containerd.UpdateContainerOpts(containerd.WithContainerLabels(info.Labels))); err != nil {
		return state, err
	}
	return containerState(ctx, container)
}
func (v *VirtualMachines) update(ctx context.Context, domain *libvirt.Domain, env string, a api.AssetExecution) (string, error) {
	active, err := domain.IsActive()
	if err != nil {
		return "unknown", err
	}
	if active {
		state, err := vmState(domain)
		if err != nil {
			return state, err
		}
		return state, fmt.Errorf("virtual machine must be stopped before updating hardware")
	}
	currentText, err := domain.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		return "unknown", err
	}
	var current libvirtxml.Domain
	if err = current.Unmarshal(currentText); err != nil {
		return "unknown", err
	}
	desiredText, err := DomainXML(env, instanceDir(v.data, env, a.InstanceId), v.bridge, a)
	if err != nil {
		return "unknown", err
	}
	var desired libvirtxml.Domain
	if err = desired.Unmarshal(desiredText); err != nil {
		return "unknown", err
	}
	desired.GenID = current.GenID
	desired.OS.Loader = current.OS.Loader
	desired.OS.NVRam = current.OS.NVRam
	desired.Devices.TPMs = current.Devices.TPMs
	sizes, err := systemDiskSizes(a)
	if err != nil {
		return "unknown", err
	}
	for index, definition := range *a.Template.Disks {
		disk, err := domainDisk(current, definition.Id)
		if err != nil {
			return "unknown", err
		}
		desired.Devices.Disks[index].Source = disk.Source
		info, err := domain.GetBlockInfo(disk.Source.File.File, 0)
		if err != nil {
			return "unknown", err
		}
		if sizes[index]*(1<<30) < int64(info.Capacity) {
			return "unknown", fmt.Errorf("system disk %s cannot be shrunk without discarding guest data", definition.Id)
		}
	}
	// Redefining hardware preserves every installed image disk and firmware/TPM state.
	for index := range sizes {
		if err = expandDisk(ctx, desired.Devices.Disks[index].Source.File.File, sizes[index]); err != nil {
			return "unknown", err
		}
	}
	if a.Asset.Volumes != nil {
		if err = v.prepareVolumes(ctx, env, a.Asset.Id, *a.Asset.Volumes); err != nil {
			return "stopped", err
		}
	}
	desiredText, err = desired.Marshal()
	if err != nil {
		return "unknown", err
	}
	updated, err := v.conn.DomainDefineXML(desiredText)
	if err != nil {
		return "unknown", err
	}
	defer updated.Free()
	return vmState(updated)
}
