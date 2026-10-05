package operation

import (
	"netlab.local/core/api"
)

func deviceBindings(node string, hardware *api.VmHardware, binding *api.PciBinding, environment, asset string, occupied map[string]string) ([]string, bool) {
	if binding == nil {
		return nil, true
	}
	if node != binding.NodeId || hardware == nil {
		return nil, false
	}
	devices := []string{}
	for _, id := range binding.GroupIds {
		found := false
		for _, group := range hardware.PciGroups {
			if group.Id != id {
				continue
			}
			owner := occupied[node+"/"+id]
			if !group.Available || owner != "" && owner != environment+"/"+asset {
				return nil, false
			}
			devices = append(devices, group.Devices...)
			found = true
			break
		}
		if !found {
			return nil, false
		}
	}
	return devices, true
}
