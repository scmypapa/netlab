//go:build linux

package engine

import (
	"fmt"
	"os/exec"
	"slices"
	"sort"
	"strings"

	"libvirt.org/go/libvirtxml"
	"netlab.local/core/api"
)

func (v *VirtualMachines) domainCapabilities(machine string) (libvirtxml.DomainCaps, error) {
	var caps libvirtxml.DomainCaps
	text, err := v.conn.GetDomainCapabilities("", "x86_64", machine, "kvm", 0)
	if err != nil {
		return caps, err
	}
	return caps, caps.Unmarshal(text)
}

func enumValues(enums []libvirtxml.DomainCapsEnum, name string) []string {
	for _, enum := range enums {
		if enum.Name == name {
			return enum.Values
		}
	}
	return nil
}

func machineCapabilities(c libvirtxml.DomainCaps) api.VmMachine {
	m := api.VmMachine{Name: c.Machine, Aliases: []string{}, Firmware: []string{}, DiskBuses: []string{}, FirmwareFiles: []string{}}
	if c.VCPU != nil {
		m.MaxVcpus = int(c.VCPU.Max)
	}
	if c.OS != nil && c.OS.Supported == "yes" && c.OS.Loader != nil && c.OS.Loader.Supported == "yes" {
		if slices.Contains(enumValues(c.OS.Loader.Enums, "type"), "rom") {
			m.Firmware = append(m.Firmware, "bios")
		}
		if slices.Contains(enumValues(c.OS.Enums, "firmware"), "efi") && slices.Contains(enumValues(c.OS.Loader.Enums, "type"), "pflash") {
			m.Firmware = append(m.Firmware, "uefi")
		}
		m.SecureBoot = slices.Contains(enumValues(c.OS.Loader.Enums, "secure"), "yes")
		m.FirmwareFiles = append(m.FirmwareFiles, c.OS.Loader.Values...)
	}
	if c.Devices != nil {
		if disk := c.Devices.Disk; disk != nil && disk.Supported == "yes" {
			for _, bus := range enumValues(disk.Enums, "bus") {
				if slices.Contains([]string{"ide", "sata", "scsi", "virtio"}, bus) {
					m.DiskBuses = append(m.DiskBuses, bus)
				}
			}
		}
		if tpm := c.Devices.TPM; tpm != nil && tpm.Supported == "yes" {
			m.Tpm2 = slices.Contains(enumValues(tpm.Enums, "model"), "tpm-crb") && slices.Contains(enumValues(tpm.Enums, "backendModel"), "emulator") && slices.Contains(enumValues(tpm.Enums, "backendVersion"), "2.0")
		}
	}
	return m
}

func (v *VirtualMachines) hardwareCapabilities() (api.VmHardware, error) {
	hardware := api.VmHardware{Machines: []api.VmMachine{}, CpuModes: []string{}, CpuModels: []string{}, NicModels: []string{}, DiskControllers: []string{}}
	_, placementErr := exec.LookPath("numad")
	hardware.NumaPlacement = ptr(placementErr == nil)
	text, err := v.conn.GetCapabilities()
	if err != nil {
		return hardware, err
	}
	var caps libvirtxml.Caps
	if err = caps.Unmarshal(text); err != nil {
		return hardware, err
	}
	machines := map[string][]string{}
	for _, guest := range caps.Guests {
		if guest.OSType != "hvm" || guest.Arch.Name != "x86_64" {
			continue
		}
		for _, domain := range guest.Arch.Domains {
			if domain.Type != "kvm" {
				continue
			}
			for _, m := range append(slices.Clone(guest.Arch.Machines), domain.Machines...) {
				name := m.Canonical
				if name == "" {
					name = m.Name
				}
				if strings.HasPrefix(name, "pc-i440fx-") || strings.HasPrefix(name, "pc-q35-") {
					aliases := machines[name]
					if name != m.Name && !slices.Contains(aliases, m.Name) {
						aliases = append(aliases, m.Name)
					}
					machines[name] = aliases
				}
			}
		}
	}
	for machine, aliases := range machines {
		caps, err := v.domainCapabilities(machine)
		if err != nil {
			return hardware, fmt.Errorf("machine %s capabilities: %w", machine, err)
		}
		capability := machineCapabilities(caps)
		capability.Aliases = append(capability.Aliases, aliases...)
		sort.Strings(capability.Aliases)
		hardware.Machines = append(hardware.Machines, capability)
	}
	sort.Slice(hardware.Machines, func(i, j int) bool { return hardware.Machines[i].Name < hardware.Machines[j].Name })
	current, err := v.domainCapabilities("")
	if err != nil {
		return hardware, err
	}
	if current.CPU != nil {
		for _, mode := range current.CPU.Modes {
			if mode.Supported != "yes" {
				continue
			}
			switch mode.Name {
			case "host-model", "host-passthrough":
				hardware.CpuModes = append(hardware.CpuModes, mode.Name)
			case "custom":
				for _, model := range mode.Models {
					if model.Usable == "yes" {
						hardware.CpuModels = append(hardware.CpuModels, model.Name)
					}
				}
			}
		}
	}
	devices, err := exec.Command(current.Path, "-device", "help").Output()
	if err != nil {
		return hardware, err
	}
	for _, nic := range []struct{ device, model string }{{"virtio-net-pci", "virtio"}, {"e1000", "e1000"}, {"e1000e", "e1000e"}, {"rtl8139", "rtl8139"}, {"vmxnet3", "vmxnet3"}} {
		if strings.Contains(string(devices), "name \""+nic.device+"\"") {
			hardware.NicModels = append(hardware.NicModels, nic.model)
		}
	}
	for _, controller := range []struct{ device, model string }{{"lsi53c895a", "lsilogic"}, {"mptsas1068", "lsisas1068"}, {"pvscsi", "vmpvscsi"}, {"virtio-scsi-pci", "virtio-scsi"}} {
		if strings.Contains(string(devices), "name \""+controller.device+"\"") {
			hardware.DiskControllers = append(hardware.DiskControllers, controller.model)
		}
	}
	return hardware, nil
}

func (v *VirtualMachines) pinHardware(t api.Template) (api.Template, error) {
	if t.Hardware == nil {
		return t, fmt.Errorf("template %s has no virtual hardware", t.Name)
	}
	caps, err := v.domainCapabilities(t.Hardware.Machine)
	if err != nil {
		return t, err
	}
	h := *t.Hardware
	h.Machine = caps.Machine
	if h.CpuModel == nil || *h.CpuModel == "" {
		h.CpuModel = ptr("host-model")
	}
	t.Hardware = &h
	return t, nil
}
