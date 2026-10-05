//go:build linux

package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"netlab.local/core/api"
)

func (e *Engine) validatePciBinding(a api.AssetExecution) error {
	if a.Asset.PciBinding == nil {
		if len(a.PciDevices) != 0 {
			return fmt.Errorf("PCI devices require a host binding")
		}
		return nil
	}
	if a.Asset.PciBinding.NodeId != e.cfg.ID {
		return fmt.Errorf("PCI binding belongs to another node")
	}
	groups, err := pciGroups("/sys")
	if err != nil {
		return err
	}
	addresses := []string{}
	for _, id := range a.Asset.PciBinding.GroupIds {
		index := slices.IndexFunc(groups, func(group api.PciGroup) bool { return group.Id == id })
		if index < 0 || !groups[index].Available {
			return fmt.Errorf("PCI group %s is not available for VFIO", id)
		}
		addresses = append(addresses, groups[index].Devices...)
	}
	slices.Sort(addresses)
	actual := slices.Sorted(slices.Values(a.PciDevices))
	if !slices.Equal(addresses, actual) {
		return fmt.Errorf("PCI execution does not include exactly the assigned IOMMU groups")
	}
	return nil
}

// An IOMMU group is the indivisible assignment boundary; only host-prepared VFIO devices are offered.
func pciGroups(root string) ([]api.PciGroup, error) {
	entries, err := os.ReadDir(filepath.Join(root, "kernel/iommu_groups"))
	if os.IsNotExist(err) {
		return []api.PciGroup{}, nil
	}
	if err != nil {
		return nil, err
	}
	groups := []api.PciGroup{}
	for _, entry := range entries {
		devices, err := os.ReadDir(filepath.Join(root, "kernel/iommu_groups", entry.Name(), "devices"))
		if err != nil {
			return nil, err
		}
		group := api.PciGroup{Devices: []string{}, Available: true}
		names := []string{}
		for _, device := range devices {
			address := device.Name()
			path := filepath.Join(root, "bus/pci/devices", address)
			vendor, err := os.ReadFile(filepath.Join(path, "vendor"))
			if err != nil {
				return nil, err
			}
			product, err := os.ReadFile(filepath.Join(path, "device"))
			if err != nil {
				return nil, err
			}
			driver, err := os.Readlink(filepath.Join(path, "driver"))
			if err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			group.Available = group.Available && filepath.Base(driver) == "vfio-pci"
			group.Devices = append(group.Devices, address)
			names = append(names, fmt.Sprintf("%s:%s", strings.TrimPrefix(strings.TrimSpace(string(vendor)), "0x"), strings.TrimPrefix(strings.TrimSpace(string(product)), "0x")))
		}
		if len(group.Devices) == 0 {
			continue
		}
		sort.Strings(group.Devices)
		group.Id = strings.Join(group.Devices, ",")
		group.Name = "PCI " + strings.Join(names, " / ")
		groups = append(groups, group)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Id < groups[j].Id })
	return groups, nil
}
