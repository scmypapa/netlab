package engine

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"path/filepath"
	"strings"

	"libvirt.org/go/libvirtxml"
	"netlab.local/core/api"
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
		OS:         &libvirtxml.DomainOS{Type: &libvirtxml.DomainOSType{Arch: "x86_64", Machine: h.Machine, Type: "hvm"}, BootDevices: []libvirtxml.DomainBootDevice{{Dev: "hd"}}},
		Features:   &libvirtxml.DomainFeatureList{ACPI: &libvirtxml.DomainFeature{}, APIC: &libvirtxml.DomainFeatureAPIC{}},
		CPU:        &libvirtxml.DomainCPU{Mode: "host-model", Topology: &libvirtxml.DomainCPUTopology{Sockets: 1, Cores: a.Asset.Resources.Cpu, Threads: 1}},
		OnPoweroff: "destroy", OnReboot: "restart", OnCrash: "destroy", Devices: &libvirtxml.DomainDeviceList{},
	}
	if h.CpuModel != nil && *h.CpuModel != "" {
		if *h.CpuModel == "host-passthrough" {
			d.CPU.Mode = "host-passthrough"
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
	diskTarget := func(i int) string {
		switch h.DiskBus {
		case api.HardwareDiskBusVirtio:
			return "vd" + string(rune('a'+i))
		case api.HardwareDiskBusIde:
			return "hd" + string(rune('a'+i))
		default:
			return "sd" + string(rune('a'+i))
		}
	}
	root := libvirtxml.DomainDisk{Device: "disk", Driver: &libvirtxml.DomainDiskDriver{Name: "qemu", Type: "qcow2", Cache: "none", Discard: "unmap"}, Source: &libvirtxml.DomainDiskSource{File: &libvirtxml.DomainDiskSourceFile{File: filepath.Join(directory, "system.qcow2")}}, Target: &libvirtxml.DomainDiskTarget{Dev: diskTarget(0), Bus: string(h.DiskBus)}}
	d.Devices.Disks = append(d.Devices.Disks, root)
	if h.DiskBus == api.HardwareDiskBusScsi {
		d.Devices.Controllers = append(d.Devices.Controllers, libvirtxml.DomainController{Type: "scsi", Model: "virtio-scsi"})
	}
	if a.Asset.Volumes != nil {
		for i, v := range *a.Asset.Volumes {
			disk := root
			disk.Source = &libvirtxml.DomainDiskSource{File: &libvirtxml.DomainDiskSourceFile{File: filepath.Join(filepath.Dir(filepath.Dir(directory)), "volumes", a.Asset.Id, v.Id+".qcow2")}}
			disk.Target = &libvirtxml.DomainDiskTarget{Dev: diskTarget(i + 1), Bus: string(h.DiskBus)}
			d.Devices.Disks = append(d.Devices.Disks, disk)
		}
	}
	for _, i := range a.Interfaces {
		iface := libvirtxml.DomainInterface{MAC: &libvirtxml.DomainInterfaceMAC{Address: i.Mac}, Source: &libvirtxml.DomainInterfaceSource{Bridge: &libvirtxml.DomainInterfaceSourceBridge{Bridge: bridge}}, Model: &libvirtxml.DomainInterfaceModel{Type: string(h.NicModel)}, VirtualPort: &libvirtxml.DomainInterfaceVirtualPort{Params: &libvirtxml.DomainInterfaceVirtualPortParams{OpenVSwitch: &libvirtxml.DomainInterfaceVirtualPortParamsOpenVSwitch{InterfaceID: i.PortName}}}, MTU: &libvirtxml.DomainInterfaceMTU{Size: uint(i.Mtu)}}
		d.Devices.Interfaces = append(d.Devices.Interfaces, iface)
	}
	d.Devices.Graphics = []libvirtxml.DomainGraphic{{VNC: &libvirtxml.DomainGraphicVNC{AutoPort: "yes", Listen: "127.0.0.1"}}}
	d.Devices.Videos = []libvirtxml.DomainVideo{{Model: libvirtxml.DomainVideoModel{Type: "vga"}}}
	d.Devices.Inputs = []libvirtxml.DomainInput{{Type: "tablet", Bus: "usb"}}
	d.Devices.Serials = []libvirtxml.DomainSerial{{Source: &libvirtxml.DomainChardevSource{Pty: &libvirtxml.DomainChardevSourcePty{}}}}
	if h.GuestAgent != nil && *h.GuestAgent {
		d.Devices.Channels = []libvirtxml.DomainChannel{{Source: &libvirtxml.DomainChardevSource{UNIX: &libvirtxml.DomainChardevSourceUNIX{Mode: "bind"}}, Target: &libvirtxml.DomainChannelTarget{VirtIO: &libvirtxml.DomainChannelTargetVirtIO{Name: "org.qemu.guest_agent.0"}}}}
	}
	if h.Tpm != nil && *h.Tpm {
		d.Devices.TPMs = []libvirtxml.DomainTPM{{Model: "tpm-crb", Backend: &libvirtxml.DomainTPMBackend{Emulator: &libvirtxml.DomainTPMBackendEmulator{Version: "2.0", PersistentState: "yes"}}}}
	}
	clock := "utc"
	if strings.Contains(strings.ToLower(a.Template.Os), "windows") {
		clock = "localtime"
	}
	d.Clock = &libvirtxml.DomainClock{Offset: clock}
	return d.Marshal()
}
