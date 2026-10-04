package guest

import (
	"fmt"

	"netlab.local/core/api"
)

// ValidateCPU keeps resource changes compatible with the template's virtual CPU layout.
func ValidateCPU(h *api.Hardware, r api.Resources) error {
	if h == nil {
		return nil
	}
	if t := h.CpuTopology; t != nil {
		if t.Sockets < 1 || t.Threads < 1 || t.Sockets > r.Cpu || t.Threads > r.Cpu || r.Cpu%t.Sockets != 0 || (r.Cpu/t.Sockets)%t.Threads != 0 {
			return fmt.Errorf("vCPU 数量应为 CPU 插槽数与每核线程数乘积的整数倍")
		}
	}
	if h.NumaNodes != nil {
		n := *h.NumaNodes
		if n < 1 || n > r.Cpu || r.Cpu%n != 0 || r.MemoryMiB%int64(n) != 0 {
			return fmt.Errorf("vCPU 和内存应能均分到 NUMA 节点")
		}
	}
	return nil
}
