package server

import (
	"context"
	"encoding/json"
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
	if ratio, err := s.Queries.GetNodeScheduling(ctx, id); err != nil || ratio != 4 {
		t.Fatal("node default CPU ratio", ratio, err)
	}
	path := "/nodes/" + id + "/scheduling"
	call("PUT", path, user, api.NodeScheduling{CpuAllocationRatio: 2}, 403)
	call("PUT", path, admin, api.NodeScheduling{CpuAllocationRatio: 0.5}, 400)
	call("PUT", "/nodes/missing/scheduling", admin, api.NodeScheduling{CpuAllocationRatio: 2}, 404)
	call("PUT", path, admin, api.NodeScheduling{CpuAllocationRatio: 2.5}, 204)
	var nodes []api.Node
	if err := json.Unmarshal(call("GET", "/nodes", admin, nil, 200), &nodes); err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		if node.Id == id {
			if node.Capacity != info.Capacity || node.CpuAllocationRatio != 2.5 {
				t.Fatal("scheduling configuration changed physical capacity", node)
			}
			return
		}
	}
	t.Fatal("configured node missing from inventory")
}
