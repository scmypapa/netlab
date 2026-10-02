//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/containerd/containerd"
	"libvirt.org/go/libvirt"
	"libvirt.org/go/libvirtxml"
	"netlab.local/core/api"
)

func (c *Containers) observedExecution(ctx context.Context, container containerd.Container) (*api.AssetExecution, error) {
	labels, err := container.Labels(ctx)
	if err != nil {
		return nil, err
	}
	var execution api.AssetExecution
	if err = json.Unmarshal([]byte(labels[executionLabel]), &execution); err != nil {
		return nil, err
	}
	spec, err := container.Spec(ctx)
	if err != nil {
		return nil, err
	}
	execution.Asset.Resources.Cpu = int(*spec.Linux.Resources.CPU.Quota / int64(*spec.Linux.Resources.CPU.Period))
	execution.Asset.Resources.MemoryMiB = *spec.Linux.Resources.Memory.Limit / (1 << 20)
	state, err := containerState(ctx, container)
	if err != nil {
		return nil, err
	}
	if state == "running" || state == "suspended" {
		task, err := container.Task(ctx, nil)
		if err != nil {
			return nil, err
		}
		cpu, memory, err := taskLimits(task.Pid())
		if err != nil {
			return nil, err
		}
		execution.Asset.Resources.Cpu = cpu
		execution.Asset.Resources.MemoryMiB = memory
	}
	return &execution, nil
}

func taskLimits(pid uint32) (int, int64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return 0, 0, err
	}
	directory := ""
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			directory = filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(line, "0::"))
		}
	}
	if directory == "" {
		return 0, 0, fmt.Errorf("the node requires unified cgroup v2")
	}
	data, err = os.ReadFile(filepath.Join(directory, "cpu.max"))
	if err != nil {
		return 0, 0, err
	}
	limits := strings.Fields(string(data))
	quota, err := strconv.ParseInt(limits[0], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	period, err := strconv.ParseInt(limits[1], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	data, err = os.ReadFile(filepath.Join(directory, "memory.max"))
	if err != nil {
		return 0, 0, err
	}
	memory, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	return int(quota / period), memory / (1 << 20), err
}

func (v *VirtualMachines) observedExecution(domain *libvirt.Domain) (*api.AssetExecution, error) {
	text, err := domain.GetXMLDesc(0)
	if err != nil {
		return nil, err
	}
	var config libvirtxml.Domain
	if err = config.Unmarshal(text); err != nil {
		return nil, err
	}
	var owner Ownership
	if err = xml.Unmarshal([]byte(config.Metadata.XML), &owner); err != nil {
		return nil, err
	}
	var execution api.AssetExecution
	if err = json.Unmarshal([]byte(owner.Execution), &execution); err != nil {
		return nil, err
	}
	hardware := *execution.Template.Hardware
	hardware.Machine = config.OS.Type.Machine
	hardware.DiskBus = api.HardwareDiskBus(config.Devices.Disks[0].Target.Bus)
	hardware.Firmware = api.Bios
	hardware.FirmwareCode, hardware.FirmwareVars = nil, nil
	secureBoot := false
	if config.OS.Loader != nil {
		hardware.FirmwareCode = ptr(config.OS.Loader.Path)
		secureBoot = config.OS.Loader.Secure == "yes"
		if config.OS.Loader.Type == "pflash" {
			hardware.Firmware = api.Uefi
		}
	}
	if config.OS.NVRam != nil {
		hardware.FirmwareVars = ptr(config.OS.NVRam.Template)
	}
	hardware.SecureBoot = &secureBoot
	hardware.Tpm = ptr(len(config.Devices.TPMs) > 0)
	guestAgent := false
	for _, channel := range config.Devices.Channels {
		guestAgent = guestAgent || channel.Target != nil && channel.Target.VirtIO != nil && channel.Target.VirtIO.Name == "org.qemu.guest_agent.0"
	}
	hardware.GuestAgent = &guestAgent
	if len(config.Devices.Interfaces) > 0 {
		hardware.NicModel = api.HardwareNicModel(config.Devices.Interfaces[0].Model.Type)
	}
	hardware.CpuModel = ptr(config.CPU.Mode)
	if config.CPU.Model != nil {
		hardware.CpuModel = ptr(config.CPU.Model.Value)
	}
	execution.Template.Hardware = &hardware
	execution.Asset.Resources.Cpu = int(config.VCPU.Value)
	info, err := domain.GetInfo()
	if err != nil {
		return nil, err
	}
	execution.Asset.Resources.MemoryMiB = int64(info.MaxMem) / 1024
	execution.Asset.Resources.DiskGiB = 0
	for index, definition := range *execution.Template.Disks {
		disk, err := domainDisk(config, definition.Id)
		if err != nil {
			return nil, err
		}
		info, err := domain.GetBlockInfo(disk.Source.File.File, 0)
		if err != nil {
			return nil, err
		}
		definition.SizeGiB = (int64(info.Capacity) + (1 << 30) - 1) / (1 << 30)
		definition.Bus = api.TemplateDiskBus(disk.Target.Bus)
		if disk.Address != nil && disk.Address.Drive != nil && disk.Address.Drive.Controller != nil {
			definition.ControllerIndex = ptr(int(*disk.Address.Drive.Controller))
			if disk.Address.Drive.Unit != nil {
				unit := int(*disk.Address.Drive.Unit)
				if definition.Bus == api.Ide && disk.Address.Drive.Bus != nil {
					unit += int(*disk.Address.Drive.Bus) * 2
				}
				definition.ControllerUnit = &unit
			}
			for _, controller := range config.Devices.Controllers {
				if controller.Type == string(definition.Bus) && controller.Index != nil && *controller.Index == *disk.Address.Drive.Controller && controller.Model != "" {
					definition.ControllerModel = ptr(controller.Model)
				}
			}
		}
		(*execution.Template.Disks)[index] = definition
		execution.Asset.Resources.DiskGiB += definition.SizeGiB
	}
	hardware.DiskController = (*execution.Template.Disks)[0].ControllerModel
	if execution.Asset.Volumes != nil {
		for index, volume := range *execution.Asset.Volumes {
			info, err := domain.GetBlockInfo(v.volumePath(owner.Environment, owner.Asset, volume.Id), 0)
			if err != nil {
				return nil, err
			}
			(*execution.Asset.Volumes)[index].SizeGiB = (int64(info.Capacity) + (1 << 30) - 1) / (1 << 30)
		}
	}
	for index, iface := range config.Devices.Interfaces {
		execution.Interfaces[index].Mac = iface.MAC.Address
		execution.Interfaces[index].PortName = iface.VirtualPort.Params.OpenVSwitch.InterfaceID
	}
	nics := make([]string, len(config.Devices.Interfaces))
	for index, iface := range config.Devices.Interfaces {
		nics[index] = iface.Model.Type
	}
	execution.Template.NicModels = &nics
	return &execution, nil
}
