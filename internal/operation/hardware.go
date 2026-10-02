package operation

import (
	"slices"

	"netlab.local/core/api"
)

func supports(info api.NodeInfo, template api.Template, cpu int) bool {
	if !slices.Contains(info.Capabilities, string(template.Kind)) {
		return false
	}
	if template.Kind == api.Container {
		return true
	}
	h, available := template.Hardware, info.VmHardware
	if h == nil || available == nil || !slices.Contains(available.NicModels, string(h.NicModel)) ||
		(h.SecureBoot != nil && *h.SecureBoot && h.Firmware != api.Uefi) {
		return false
	}
	model := "host-model"
	if h.CpuModel != nil && *h.CpuModel != "" {
		model = *h.CpuModel
	}
	if !slices.Contains(available.CpuModes, model) && !slices.Contains(available.CpuModels, model) {
		return false
	}
	for _, machine := range available.Machines {
		if machine.Name != h.Machine && !slices.Contains(machine.Aliases, h.Machine) {
			continue
		}
		return cpu <= machine.MaxVcpus && slices.Contains(machine.Firmware, string(h.Firmware)) &&
			slices.Contains(machine.DiskBuses, string(h.DiskBus)) &&
			(h.SecureBoot == nil || !*h.SecureBoot || machine.SecureBoot) &&
			(h.Tpm == nil || !*h.Tpm || machine.Tpm2) &&
			(h.FirmwareCode == nil || *h.FirmwareCode == "" || slices.Contains(machine.FirmwareFiles, *h.FirmwareCode))
	}
	return false
}
