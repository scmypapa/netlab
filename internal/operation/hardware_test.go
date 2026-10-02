package operation

import (
	"testing"

	"netlab.local/core/api"
)

func TestPlacementUsesRequiredHardware(t *testing.T) {
	container := api.Template{Kind: api.Container}
	onlyContainers := api.NodeInfo{Capabilities: []string{"container", "network"}}
	if !supports(onlyContainers, container, 2) {
		t.Fatal("container scheduling depends on unrelated VM capabilities")
	}
	secure := true
	vm := api.Template{Kind: api.Vm, Hardware: &api.Hardware{Machine: "q35", Firmware: api.Uefi, DiskBus: api.HardwareDiskBusSata, NicModel: api.HardwareNicModelE1000, SecureBoot: &secure, Tpm: &secure}}
	node := api.NodeInfo{Capabilities: []string{"vm"}, VmHardware: &api.VmHardware{
		CpuModes: []string{"host-model"}, NicModels: []string{"e1000"},
		Machines: []api.VmMachine{{Name: "pc-q35-8.2", Aliases: []string{"q35"}, MaxVcpus: 16, Firmware: []string{"bios", "uefi"}, DiskBuses: []string{"sata"}, SecureBoot: true, Tpm2: true}},
	}}
	if supports(onlyContainers, vm, 2) || !supports(node, vm, 2) {
		t.Fatal("hardware selection does not distinguish container and modern VM nodes")
	}
	node.VmHardware.Machines[0].SecureBoot = false
	if supports(node, vm, 2) {
		t.Fatal("selected a node without required Secure Boot")
	}
	node.VmHardware.Machines[0].SecureBoot = true
	vm.Hardware.Machine = "pc-q35-8.2"
	if !supports(node, vm, 16) || supports(node, vm, 17) {
		t.Fatal("canonical machine or vCPU limit was ignored")
	}
	vm.Hardware.Firmware = api.Bios
	if supports(node, vm, 2) {
		t.Fatal("Secure Boot was accepted with BIOS")
	}
}
