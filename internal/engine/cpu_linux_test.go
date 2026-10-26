//go:build linux

package engine

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/containerd/containerd/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestFractionalContainerCPULimit(t *testing.T) {
	for _, cpu := range []float64{0.01, 0.1, 0.25, 0.29, 0.5, 1, 2.75} {
		spec := &oci.Spec{Linux: &specs.Linux{}}
		if err := cpuLimit(cpu)(context.Background(), nil, nil, spec); err != nil {
			t.Fatal(err)
		}
		limit := spec.Linux.Resources.CPU
		if float64(*limit.Quota)/float64(*limit.Period) != cpu {
			t.Fatalf("CPU=%g quota=%d period=%d", cpu, *limit.Quota, *limit.Period)
		}
	}
}

func TestRealFractionalCgroupObservation(t *testing.T) {
	input := os.Getenv("NETLAB_TEST_CGROUP_CPU")
	if input == "" {
		t.Skip("NETLAB_TEST_CGROUP_CPU selects the test process cgroup limit")
	}
	want, err := strconv.ParseFloat(input, 64)
	if err != nil {
		t.Fatal(err)
	}
	cpu, memory, err := taskLimits(uint32(os.Getpid()))
	if err != nil || cpu != want || memory != 128 {
		t.Fatalf("cgroup observation: CPU=%g memory=%d error=%v", cpu, memory, err)
	}
}
