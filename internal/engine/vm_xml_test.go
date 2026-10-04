package engine

import (
	"encoding/xml"
	"path/filepath"
	"testing"

	"libvirt.org/go/libvirtxml"
	"netlab.local/core/api"
)

func TestVMHardwareProfiles(t *testing.T) {
	for _, profile := range []struct {
		name        string
		os          string
		firmware    api.HardwareFirmware
		machine     string
		bus         api.HardwareDiskBus
		nic         api.HardwareNicModel
		tpm, secure bool
	}{
		{"legacy", "Windows 7", api.Bios, "pc-i440fx-8.2", api.HardwareDiskBusIde, api.HardwareNicModelE1000, false, false},
		{"modern", "Windows Server 2025", api.Uefi, "pc-q35-8.2", api.HardwareDiskBusSata, api.HardwareNicModelE1000e, true, true},
		{"linux", "Linux", api.Uefi, "pc-q35-8.2", api.HardwareDiskBusSata, api.HardwareNicModelE1000e, false, false},
	} {
		t.Run(profile.name, func(t *testing.T) {
			disks := []api.TemplateDisk{{Id: "boot", SizeGiB: 20, Bus: api.TemplateDiskBus(profile.bus), BootOrder: 1}}
			exec := api.AssetExecution{InstanceId: "8193951c-e317-49ee-95e9-5c4228938d18", Asset: api.Asset{Id: "vm", Name: "Guest", Resources: api.Resources{Cpu: 2, MemoryMiB: 4096, DiskGiB: 20}}, Template: api.Template{Disks: &disks, Hardware: &api.Hardware{Firmware: profile.firmware, Machine: profile.machine, DiskBus: profile.bus, NicModel: profile.nic, Tpm: &profile.tpm, SecureBoot: &profile.secure}}, Interfaces: []api.ResolvedInterface{{Id: "business", PortName: "8f23044c-3a85-4a5b-9740-95a79973f655", Mac: "02:00:01:02:03:04", Mtu: 1400}}}
			exec.Template.Os = profile.os
			document, err := DomainXML("environment", "/var/lib/netlab/environments/environment/instances/instance", "br-int", exec)
			if err != nil {
				t.Fatal(err)
			}
			var domain libvirtxml.Domain
			if err = domain.Unmarshal(document); err != nil {
				t.Fatal(err)
			}
			if domain.Clock == nil || domain.Clock.Offset != "utc" {
				t.Fatal("virtual RTC depends on the node timezone")
			}
			if domain.OS.Type.Machine != profile.machine || domain.Devices.Disks[0].Target.Bus != string(profile.bus) {
				t.Fatal("template hardware changed")
			}
			if len(domain.Devices.Interfaces) != 1 || domain.Devices.Interfaces[0].VirtualPort.Params.OpenVSwitch.InterfaceID != exec.Interfaces[0].PortName {
				t.Fatal("unexpected or unstable business interface")
			}
			if profile.firmware == api.Uefi && (domain.OS.NVRam == nil || domain.OS.NVRam.NVRam != filepath.Join("/var/lib/netlab/environments/environment/instances/instance", "nvram.fd")) {
				t.Fatal("NVRAM is not instance-owned")
			}
			if len(domain.Devices.TPMs) > 0 != profile.tpm {
				t.Fatal("TPM does not match hardware")
			}
			var ownership Ownership
			if err = xml.Unmarshal([]byte(domain.Metadata.XML), &ownership); err != nil {
				t.Fatal(err)
			}
			if ownership.Asset != "vm" || ownership.Instance != exec.InstanceId {
				t.Fatal("runtime ownership missing")
			}
		})
	}
}
