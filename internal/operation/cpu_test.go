package operation

import (
	"encoding/json"
	"testing"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
)

func TestCPUAllocationDoesNotChangePhysicalCapacity(t *testing.T) {
	info := api.NodeInfo{Capacity: api.Resources{Cpu: 2, MemoryMiB: 4096, DiskGiB: 100}}
	capacity := SchedulingCapacity(info, 4, 0)
	if capacity.Cpu != 8 || info.Capacity.Cpu != 2 || info.Capacity.MemoryMiB != 4096 || capacity.MemoryMiB != 3072 || capacity.DiskGiB != 100 {
		t.Fatal("physical capacity or non-CPU resources changed", info.Capacity, capacity)
	}
	if !fits(api.Resources{Cpu: 8, MemoryMiB: 3072}, capacity) || fits(api.Resources{Cpu: 8.01}, capacity) || fits(api.Resources{Cpu: 1, MemoryMiB: 3073}, capacity) {
		t.Fatal("CPU over-allocation or memory boundary ignored")
	}
	used := api.Resources{}
	for range 30 {
		used = add(used, api.Resources{Cpu: 0.1})
	}
	if used.Cpu != 3 || !fits(used, api.Resources{Cpu: 3}) {
		t.Fatal("fractional CPU accumulation changed admission", used)
	}
}

func TestHostMemoryCapacityPolicy(t *testing.T) {
	for _, test := range []struct {
		total, reserve, allocatable int64
	}{
		{1024, 0, 512}, {2048, 0, 1024}, {16384, 0, 14746},
	} {
		info := api.NodeInfo{Capacity: api.Resources{Cpu: 8, MemoryMiB: test.total}}
		got := SchedulingCapacity(info, 4, test.reserve)
		if got.MemoryMiB != test.allocatable || info.Capacity.MemoryMiB != test.total {
			t.Fatalf("total=%d configured=%d: allocatable=%d, want %d", test.total, test.reserve, got.MemoryMiB, test.allocatable)
		}
	}
}

func TestMigrationUsesFractionalCPUAllocation(t *testing.T) {
	info := api.NodeInfo{Capacity: api.Resources{Cpu: 2, MemoryMiB: 4096, DiskGiB: 100}, Capabilities: []string{"container"}}
	raw, _ := json.Marshal(info)
	nodes := []queries.ListNodesRow{{ID: "target", Name: "Target", State: "ready", Info: raw, CpuAllocationRatio: 4}}
	source := Target{NodeID: "source", Execution: api.AssetExecution{Template: api.Template{Kind: api.Container}, Asset: api.Asset{Resources: api.Resources{Cpu: 0.25, MemoryMiB: 128}}}}
	reserved := []queries.GetNodeReservationsRow{{NodeID: "target", Cpu: 7.75, MemoryMib: 512}}
	destinations, err := migrationCandidates(source, nodes, nil, reserved)
	if err != nil || len(destinations) != 1 || destinations[0].Available.Cpu != 0.25 {
		t.Fatalf("fractional CPU destination lost: %+v %v", destinations, err)
	}
	reserved[0].Cpu = 7.76
	if destinations, err = migrationCandidates(source, nodes, nil, reserved); err != nil || len(destinations) != 0 {
		t.Fatalf("CPU budget exceeded: %+v %v", destinations, err)
	}
	reserved[0] = queries.GetNodeReservationsRow{NodeID: "target", Cpu: 1, MemoryMib: 4000}
	if destinations, err = migrationCandidates(source, nodes, nil, reserved); err != nil || len(destinations) != 0 {
		t.Fatalf("CPU over-allocation bypassed memory admission: %+v %v", destinations, err)
	}
	source.NodeID = "target"
	nodes[0].CpuAllocationRatio = 1
	reserved[0] = queries.GetNodeReservationsRow{NodeID: "target", Cpu: 7.75, MemoryMib: 512}
	if destinations, err = migrationCandidates(source, nodes, nil, reserved); err != nil || len(destinations) != 1 || destinations[0].Available.Cpu != 0 {
		t.Fatalf("lowering CPU ratio blocked same-node storage migration: %+v %v", destinations, err)
	}
}
