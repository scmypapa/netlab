package operation

import (
	"slices"
	"testing"

	"netlab.local/core/api"
)

func TestDeviceBindingOwnership(t *testing.T) {
	group := api.PciGroup{Id: "group", Available: true, Devices: []string{"0000:03:00.0", "0000:03:00.1"}}
	hardware := &api.VmHardware{PciGroups: []api.PciGroup{group}}
	binding := &api.PciBinding{NodeId: "node", GroupIds: []string{group.Id}}
	for _, owner := range []string{"", "env/asset"} {
		devices, ok := deviceBindings("node", hardware, binding, "env", "asset", map[string]string{"node/group": owner})
		if !ok || !slices.Equal(devices, group.Devices) {
			t.Fatal("whole-group assignment or replacement failed", devices, ok)
		}
	}
	for _, owner := range []string{"other/asset", "env/other"} {
		if _, ok := deviceBindings("node", hardware, binding, "env", "asset", map[string]string{"node/group": owner}); ok {
			t.Fatal("device double assigned", owner)
		}
	}
	if _, ok := deviceBindings("another-node", hardware, binding, "env", "asset", nil); ok {
		t.Fatal("host binding ignored")
	}
	hardware.PciGroups[0].Available = false
	if _, ok := deviceBindings("node", hardware, binding, "env", "asset", nil); ok {
		t.Fatal("unprepared device assigned")
	}
}
