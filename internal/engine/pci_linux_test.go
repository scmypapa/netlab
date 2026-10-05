//go:build linux

package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIOMMUGroupDiscovery(t *testing.T) {
	root := t.TempDir()
	for _, address := range []string{"0000:03:00.0", "0000:03:00.1"} {
		device := filepath.Join(root, "bus/pci/devices", address)
		group := filepath.Join(root, "kernel/iommu_groups/7/devices")
		for _, path := range []string{device, group} {
			if err := os.MkdirAll(path, 0755); err != nil {
				t.Fatal(err)
			}
		}
		for file, value := range map[string]string{"vendor": "0x1234\n", "device": "0x5678\n"} {
			if err := os.WriteFile(filepath.Join(device, file), []byte(value), 0644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink("../../../drivers/vfio-pci", filepath.Join(device, "driver")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(device, filepath.Join(group, address)); err != nil {
			t.Fatal(err)
		}
	}
	groups, err := pciGroups(root)
	if err != nil || len(groups) != 1 || !groups[0].Available || groups[0].Id != "0000:03:00.0,0000:03:00.1" {
		t.Fatal(groups, err)
	}
	if err := os.Rename(filepath.Join(root, "kernel/iommu_groups/7"), filepath.Join(root, "kernel/iommu_groups/19")); err != nil {
		t.Fatal(err)
	}
	current, err := pciGroups(root)
	if err != nil || current[0].Id != groups[0].Id {
		t.Fatal("unstable group identity", current, err)
	}
	if err := os.Remove(filepath.Join(root, "bus/pci/devices/0000:03:00.1/driver")); err != nil {
		t.Fatal(err)
	}
	current, err = pciGroups(root)
	if err != nil || current[0].Available {
		t.Fatal("partially prepared group offered", current, err)
	}
}
