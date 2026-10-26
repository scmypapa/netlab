package guest

import (
	"fmt"
	"math"

	"netlab.local/core/api"
)

// ValidateCPU keeps resource changes compatible with the template's virtual CPU layout.
func ValidateCPU(kind api.TemplateKind, h *api.Hardware, r api.Resources) error {
	if math.IsNaN(r.Cpu) || math.IsInf(r.Cpu, 0) || r.Cpu < 0.01 || r.Cpu != math.Round(r.Cpu*100)/100 {
		return fmt.Errorf("CPU 额度应至少为 0.01 核，精确到 0.01 核")
	}
	if kind == api.Vm && r.Cpu != math.Trunc(r.Cpu) {
		return fmt.Errorf("虚拟机 vCPU 数量应为正整数")
	}
	if h == nil {
		return nil
	}
	cpu := int(r.Cpu)
	if t := h.CpuTopology; t != nil {
		if t.Sockets < 1 || t.Threads < 1 || t.Sockets > cpu || t.Threads > cpu || cpu%t.Sockets != 0 || (cpu/t.Sockets)%t.Threads != 0 {
			return fmt.Errorf("vCPU 数量应为 CPU 插槽数与每核线程数乘积的整数倍")
		}
	}
	if h.NumaNodes != nil {
		n := *h.NumaNodes
		if n < 1 || n > cpu || cpu%n != 0 || r.MemoryMiB%int64(n) != 0 {
			return fmt.Errorf("vCPU 和内存应能均分到 NUMA 节点")
		}
	}
	return nil
}
