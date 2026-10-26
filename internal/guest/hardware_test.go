package guest

import (
	"math"
	"testing"

	"netlab.local/core/api"
)

func TestCPUResourceCompatibility(t *testing.T) {
	nodes := 2
	h := &api.Hardware{CpuTopology: &api.CpuTopology{Sockets: 2, Threads: 2}, NumaNodes: &nodes}
	for _, sample := range []struct {
		cpu    float64
		memory int64
		valid  bool
	}{{8, 4096, true}, {4, 2048, true}, {6, 4096, false}, {8, 4097, false}, {1, 4096, false}} {
		err := ValidateCPU(api.Vm, h, api.Resources{Cpu: sample.cpu, MemoryMiB: sample.memory})
		if (err == nil) != sample.valid {
			t.Fatalf("CPU=%g memory=%d: %v", sample.cpu, sample.memory, err)
		}
	}
}

func TestFractionalCPU(t *testing.T) {
	for _, sample := range []struct {
		kind  api.TemplateKind
		cpu   float64
		valid bool
	}{
		{api.Container, 0.01, true}, {api.Container, 0.1, true}, {api.Container, 0.29, true}, {api.Container, 0.5, true},
		{api.Container, 0, false}, {api.Container, -1, false}, {api.Container, 0.001, false}, {api.Container, 1.001, false},
		{api.Container, math.NaN(), false}, {api.Container, math.Inf(1), false},
		{api.Vm, 1, true}, {api.Vm, 8, true}, {api.Vm, 0.5, false}, {api.Vm, 1.5, false},
	} {
		if err := ValidateCPU(sample.kind, nil, api.Resources{Cpu: sample.cpu}); (err == nil) != sample.valid {
			t.Fatalf("kind=%s CPU=%g valid=%t: %v", sample.kind, sample.cpu, sample.valid, err)
		}
	}
}
