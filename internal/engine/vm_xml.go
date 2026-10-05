package engine

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"path/filepath"
	"slices"

	"libvirt.org/go/libvirtxml"
	"netlab.local/core/api"
	"netlab.local/core/internal/guest"
)

type Ownership struct {
	XMLName     xml.Name `xml:"urn:netlab:instance instance"`
	Environment string   `xml:"environment,attr"`
	Asset       string   `xml:"asset,attr"`
	Instance    string   `xml:"instance,attr"`
	Execution   string   `xml:"execution"`
}

func DomainXML(environmentID, directory, bridge string, a api.AssetExecution) (string, error) {
	if a.Template.Hardware == nil {
		return "", fmt.Errorf("template %s has no virtual hardware", a.Template.Name)
	}
	h := a.Template.Hardware
	if err := guest.ValidateCPU(h, a.Asset.Resources); err != nil {
		return "", err
	}
	if a.Template.Disks == nil || len(*a.Template.Disks) == 0 {
		return "", fmt.Errorf("template %s has no prepared system disks", a.Template.Name)
	}
	execution, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	metadata, err := xml.Marshal(Ownership{Environment: environmentID, Asset: a.Asset.Id, Instance: a.InstanceId, Execution: string(execution)})
	if err != nil {
		return "", err
	}
	d := libvirtxml.Domain{Type: "kvm", Name: "netlab-" + a.InstanceId, UUID: a.InstanceId, GenID: &libvirtxml.DomainGenID{},
		Title: a.Asset.Name, Metadata: &libvirtxml.DomainMetadata{XML: string(metadata)},
		Memory: &libvirtxml.DomainMemory{Value: uint(a.Asset.Resources.MemoryMiB), Unit: "MiB"}, VCPU: &libvirtxml.DomainVCPU{Value: uint(a.Asset.Resources.Cpu)},
		OS:         &libvirtxml.DomainOS{Type: &libvirtxml.DomainOSType{Arch: "x86_64", Machine: h.Machine, Type: "hvm"}},
		Features:   &libvirtxml.DomainFeatureList{ACPI: &libvirtxml.DomainFeature{}, APIC: &libvirtxml.DomainFeatureAPIC{}},
		CPU:        &libvirtxml.DomainCPU{Mode: "host-model", Topology: &libvirtxml.DomainCPUTopology{Sockets: 1, Cores: a.Asset.Resources.Cpu, Threads: 1}},
		OnPoweroff: "destroy", OnReboot: "restart", OnCrash: "destroy", Devices: &libvirtxml.DomainDeviceList{},
	}
	if t := h.CpuTopology; t != nil {
		d.CPU.Topology = &libvirtxml.DomainCPUTopology{Sockets: t.Sockets, Threads: t.Threads, Cores: a.Asset.Resources.Cpu / t.Sockets / t.Threads}
	}
	if d.Devices.Hostdevs, err = domainHostDevices(a.PciDevices); err != nil {
		return "", err
	}
	if h.NumaPlacement != nil {
		d.VCPU.Placement = "auto"
		d.NUMATune = &libvirtxml.DomainNUMATune{Memory: &libvirtxml.DomainNUMATuneMemory{Mode: string(*h.NumaPlacement), Placement: "auto"}}
	}
	if h.NumaNodes != nil && *h.NumaNodes > 1 {
		d.CPU.Numa = &libvirtxml.DomainNuma{}
		nodes := *h.NumaNodes
		cpus := a.Asset.Resources.Cpu / nodes
		for i := range nodes {
			id := uint(i)
			d.CPU.Numa.Cell = append(d.CPU.Numa.Cell, libvirtxml.DomainCell{ID: &id, CPUs: fmt.Sprintf("%d-%d", i*cpus, (i+1)*cpus-1), Memory: uint(a.Asset.Resources.MemoryMiB / int64(nodes)), Unit: "MiB"})
		}
	}
	if h.CpuModel != nil && *h.CpuModel != "" {
		if *h.CpuModel == "host-passthrough" || *h.CpuModel == "host-model" {
			d.CPU.Mode = *h.CpuModel
		} else {
			d.CPU.Mode = "custom"
			d.CPU.Match = "exact"
			d.CPU.Model = &libvirtxml.DomainCPUModel{Value: *h.CpuModel, Fallback: "forbid"}
		}
	}
	if h.Firmware == api.Uefi {
		d.OS.Firmware = "efi"
		secure := "no"
		if h.SecureBoot != nil && *h.SecureBoot {
			secure = "yes"
			d.Features.SMM = &libvirtxml.DomainFeatureSMM{State: "on"}
		}
		d.OS.FirmwareInfo = &libvirtxml.DomainOSFirmwareInfo{Features: []libvirtxml.DomainOSFirmwareFeature{{Name: "secure-boot", Enabled: secure}, {Name: "enrolled-keys", Enabled: secure}}}
		d.OS.NVRam = &libvirtxml.DomainNVRam{NVRam: filepath.Join(directory, "nvram.fd")}
		if h.FirmwareVars != nil {
			d.OS.NVRam.Template = *h.FirmwareVars
		}
		if h.FirmwareCode != nil {
			d.OS.Loader = &libvirtxml.DomainLoader{Path: *h.FirmwareCode, Readonly: "yes", Type: "pflash", Secure: secure}
		}
	}
	controllers := map[string]string{}
	units := map[string]uint{}
	targets := map[string]int{}
	appendDisk := func(location vmDisk, definition api.TemplateDisk, boot bool) error {
		bus := string(definition.Bus)
		prefix := "sd"
		if bus == "virtio" {
			prefix = "vd"
		} else if bus == "ide" {
			prefix = "hd"
		}
		device := diskDevice(prefix, targets[prefix])
		targets[prefix]++
		serial := "volume-" + definition.Id
		if boot {
			serial = "image-" + definition.Id
		}
		source, auth, err := location.source()
		if err != nil {
			return err
		}
		disk := libvirtxml.DomainDisk{Device: "disk", Serial: serial, Driver: &libvirtxml.DomainDiskDriver{Name: "qemu", Type: location.format(), Cache: "none", Discard: "unmap"}, Source: source, Auth: auth, Target: &libvirtxml.DomainDiskTarget{Dev: device, Bus: bus}}
		if boot {
			disk.Boot = &libvirtxml.DomainDeviceBoot{Order: uint(definition.BootOrder)}
		}
		if bus != "virtio" {
			index := uint(0)
			if definition.ControllerIndex != nil {
				index = uint(*definition.ControllerIndex)
			}
			model := ""
			if bus == "scsi" {
				model = "virtio-scsi"
				if definition.ControllerModel != nil {
					model = *definition.ControllerModel
				}
			}
			key := fmt.Sprintf("%s/%d", bus, index)
			if existing, ok := controllers[key]; ok {
				if existing != model {
					return fmt.Errorf("disk controller %s has conflicting models", key)
				}
			} else {
				d.Devices.Controllers = append(d.Devices.Controllers, libvirtxml.DomainController{Type: bus, Index: &index, Model: model})
				controllers[key] = model
			}
			zero, unit := uint(0), units[key]
			if definition.ControllerUnit != nil {
				unit = uint(*definition.ControllerUnit)
			}
			units[key] = max(units[key], unit+1)
			driveBus := uint(0)
			if bus == "ide" {
				driveBus, unit = unit/2, unit%2
			}
			disk.Address = &libvirtxml.DomainAddress{Drive: &libvirtxml.DomainAddressDrive{Controller: &index, Bus: &driveBus, Target: &zero, Unit: &unit}}
		}
		d.Devices.Disks = append(d.Devices.Disks, disk)
		return nil
	}
	for index, disk := range *a.Template.Disks {
		if err := appendDisk(systemDisk(directory, a, index), disk, true); err != nil {
			return "", err
		}
	}
	if a.Asset.Volumes != nil {
		for _, v := range *a.Asset.Volumes {
			if err := appendDisk(volumeDisk(directory, environmentID, a, v.Id), api.TemplateDisk{Id: v.Id, Bus: api.TemplateDiskBus(h.DiskBus), ControllerModel: h.DiskController, ControllerIndex: (*a.Template.Disks)[0].ControllerIndex}, false); err != nil {
				return "", err
			}
		}
	}
	if initializationMethod(a.Template) != api.None {
		if err := appendDisk(vmDisk{file: filepath.Join(directory, "initialization.iso")}, api.TemplateDisk{Id: "initialization", Bus: api.Sata}, false); err != nil {
			return "", err
		}
		media := &d.Devices.Disks[len(d.Devices.Disks)-1]
		media.Device, media.Driver.Type, media.Serial = "cdrom", "raw", ""
		media.ReadOnly = &libvirtxml.DomainDiskReadOnly{}
	}
	if a.Template.Media != nil {
		order := uint(1)
		for _, disk := range *a.Template.Disks {
			order = max(order, uint(disk.BootOrder)+1)
		}
		for index, media := range *a.Template.Media {
			if a.Asset.Media != nil && !slices.Contains(*a.Asset.Media, media.Id) {
				continue
			}
			path := templateMediaPath(directory, index)
			if err := appendDisk(vmDisk{file: path}, api.TemplateDisk{Id: media.Id, Bus: api.Sata}, false); err != nil {
				return "", err
			}
			disk := &d.Devices.Disks[len(d.Devices.Disks)-1]
			disk.Device, disk.Driver.Type, disk.Serial = "cdrom", "raw", ""
			disk.ReadOnly = &libvirtxml.DomainDiskReadOnly{}
			disk.Boot = &libvirtxml.DomainDeviceBoot{Order: order}
			order++
		}
	}
	for index, i := range a.Interfaces {
		model := string(h.NicModel)
		if a.Template.NicModels != nil && index < len(*a.Template.NicModels) {
			model = (*a.Template.NicModels)[index]
		}
		iface := libvirtxml.DomainInterface{MAC: &libvirtxml.DomainInterfaceMAC{Address: i.Mac}, Source: &libvirtxml.DomainInterfaceSource{Bridge: &libvirtxml.DomainInterfaceSourceBridge{Bridge: bridge}}, Model: &libvirtxml.DomainInterfaceModel{Type: model}, VirtualPort: &libvirtxml.DomainInterfaceVirtualPort{Params: &libvirtxml.DomainInterfaceVirtualPortParams{OpenVSwitch: &libvirtxml.DomainInterfaceVirtualPortParamsOpenVSwitch{InterfaceID: i.PortName}}}, MTU: &libvirtxml.DomainInterfaceMTU{Size: uint(i.Mtu)}}
		d.Devices.Interfaces = append(d.Devices.Interfaces, iface)
	}
	d.Devices.Graphics = []libvirtxml.DomainGraphic{{VNC: &libvirtxml.DomainGraphicVNC{AutoPort: "yes", Listen: "127.0.0.1"}}}
	d.Devices.Videos = []libvirtxml.DomainVideo{{Model: libvirtxml.DomainVideoModel{Type: "vga"}}}
	d.Devices.Inputs = []libvirtxml.DomainInput{{Type: "tablet", Bus: "usb"}}
	d.Devices.Serials = []libvirtxml.DomainSerial{{Source: &libvirtxml.DomainChardevSource{Pty: &libvirtxml.DomainChardevSourcePty{}}}}
	if h.GuestAgent == nil || *h.GuestAgent {
		d.Devices.Channels = []libvirtxml.DomainChannel{{Source: &libvirtxml.DomainChardevSource{UNIX: &libvirtxml.DomainChardevSourceUNIX{Mode: "bind"}}, Target: &libvirtxml.DomainChannelTarget{VirtIO: &libvirtxml.DomainChannelTargetVirtIO{Name: "org.qemu.guest_agent.0"}}}}
	}
	if h.Tpm != nil && *h.Tpm {
		d.Devices.TPMs = []libvirtxml.DomainTPM{{Model: "tpm-crb", Backend: &libvirtxml.DomainTPMBackend{Emulator: &libvirtxml.DomainTPMBackendEmulator{Version: "2.0", PersistentState: "yes"}}}}
	}
	d.Clock = &libvirtxml.DomainClock{Offset: "utc"}
	return d.Marshal()
}

func templateMediaPath(directory string, index int) string {
	return filepath.Join(directory, fmt.Sprintf("media-%d.iso", index))
}

func systemDiskPath(directory string, index int) string {
	return filepath.Join(directory, fmt.Sprintf("disk-%d.qcow2", index))
}

func systemDiskSizes(a api.AssetExecution) ([]int64, error) {
	if a.Template.Disks == nil || len(*a.Template.Disks) == 0 {
		return nil, fmt.Errorf("template %s has no prepared system disks", a.Template.Name)
	}
	disks := *a.Template.Disks
	sizes, total, boot := make([]int64, len(disks)), int64(0), 0
	for index, disk := range disks {
		sizes[index] = disk.SizeGiB
		total += disk.SizeGiB
		if disk.BootOrder < disks[boot].BootOrder {
			boot = index
		}
	}
	if a.Asset.Resources.DiskGiB < total {
		return nil, fmt.Errorf("requested system disk capacity %d GiB is below template capacity %d GiB", a.Asset.Resources.DiskGiB, total)
	}
	sizes[boot] += a.Asset.Resources.DiskGiB - total
	return sizes, nil
}

func diskDevice(prefix string, index int) string {
	name := ""
	for index >= 0 {
		name = string(rune('a'+index%26)) + name
		index = index/26 - 1
	}
	return prefix + name
}

func domainDisk(config libvirtxml.Domain, serial string) (libvirtxml.DomainDisk, error) {
	for _, disk := range config.Devices.Disks {
		if disk.Serial == serial {
			return disk, nil
		}
	}
	return libvirtxml.DomainDisk{}, fmt.Errorf("installed disk %s is missing", serial)
}
