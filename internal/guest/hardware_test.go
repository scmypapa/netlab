package guest

import (
	"testing"

	"netlab.local/core/api"
)

func TestCPUResourceCompatibility(t *testing.T) {
	nodes := 2
	h := &api.Hardware{CpuTopology: &api.CpuTopology{Sockets: 2, Threads: 2}, NumaNodes: &nodes}
	for _, sample := range []struct {
		cpu    int
		memory int64
		valid  bool
	}{{8, 4096, true}, {4, 2048, true}, {6, 4096, false}, {8, 4097, false}, {1, 4096, false}} {
		err := ValidateCPU(h, api.Resources{Cpu: sample.cpu, MemoryMiB: sample.memory})
		if (err == nil) != sample.valid {
			t.Fatalf("CPU=%d memory=%d: %v", sample.cpu, sample.memory, err)
		}
	}
}
