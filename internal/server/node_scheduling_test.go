package server

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
)

func testNodeSchedulingAPI(t *testing.T, ctx context.Context, s *Server, admin, user string, call func(string, string, string, any, int) []byte) {
	t.Helper()
	id := uuid.NewString()
	info := api.NodeInfo{Id: id, Capacity: api.Resources{Cpu: 2, MemoryMiB: 4096, DiskGiB: 100}}
	raw, _ := json.Marshal(info)
	if err := s.Queries.PutNode(ctx, queries.PutNodeParams{ID: id, Name: "CPU allocation", Endpoint: "https://cpu.test", Info: raw}); err != nil {
		t.Fatal(err)
	}
	if setting, err := s.Queries.GetNodeScheduling(ctx, id); err != nil || setting.CpuAllocationRatio != 4 || setting.MemoryReserveMib != 0 {
		t.Fatal("node default scheduling", setting, err)
	}
	path := "/nodes/" + id + "/scheduling"
	call("PUT", path, user, api.NodeScheduling{CpuAllocationRatio: 2}, 403)
	call("PUT", path, admin, api.NodeScheduling{CpuAllocationRatio: 0.5}, 400)
	call("PUT", "/nodes/missing/scheduling", admin, api.NodeScheduling{CpuAllocationRatio: 2}, 404)
	call("PUT", path, admin, api.NodeScheduling{CpuAllocationRatio: 2.5}, 204)
	for _, reserve := range []int64{-1, 4096, 4097} {
		call("PUT", path, admin, api.NodeScheduling{CpuAllocationRatio: 2.5, MemoryReserveMiB: &reserve}, 400)
	}
	reserve := int64(512)
	call("PUT", path, admin, api.NodeScheduling{CpuAllocationRatio: 2.5, MemoryReserveMiB: &reserve}, 204)
	call("PUT", path, admin, api.NodeScheduling{CpuAllocationRatio: 3}, 204)
	if setting, err := s.Queries.GetNodeScheduling(ctx, id); err != nil || setting.MemoryReserveMib != reserve || setting.CpuAllocationRatio != 3 {
		t.Fatal("CPU-only client erased the host memory reservation", setting, err)
	}
	inventory := func() api.Node {
		var nodes []api.Node
		if err := json.Unmarshal(call("GET", "/nodes", admin, nil, 200), &nodes); err != nil {
			t.Fatal(err)
		}
		index := slices.IndexFunc(nodes, func(node api.Node) bool { return node.Id == id })
		if index < 0 {
			t.Fatal("configured node missing from inventory")
		}
		return nodes[index]
	}
	if err := s.Queries.PutNode(ctx, queries.PutNodeParams{ID: id, Name: "Re-registered", Endpoint: "https://cpu.test", Info: raw}); err != nil {
		t.Fatal(err)
	}
	node := inventory()
	if node.Capacity != info.Capacity || node.CpuAllocationRatio != 3 || node.MemoryReserveMiB != reserve || node.AllocatableMemoryMiB != 3584 {
		t.Fatal("registration did not preserve scheduling configuration and physical capacity", node)
	}
	reserve = 0
	call("PUT", path, admin, api.NodeScheduling{CpuAllocationRatio: 3, MemoryReserveMiB: &reserve}, 204)
	node = inventory()
	if node.MemoryReserveMiB != 0 || node.AllocatableMemoryMiB != 3072 {
		t.Fatal("reset did not restore automatic host reservation", node)
	}
}
