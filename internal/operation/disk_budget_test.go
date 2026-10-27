package operation

import (
	"encoding/json"
	"reflect"
	"testing"

	"netlab.local/core/api"
)

func TestDiskBudgetPeaksKeepRealExecution(t *testing.T) {
	before := api.Asset{Id: "server", Resources: api.Resources{DiskGiB: 2}, Volumes: &[]api.Volume{{Id: "data", SizeGiB: 4}}}
	after := api.Asset{Id: "server", Resources: api.Resources{DiskGiB: 3}, Volumes: &[]api.Volume{{Id: "data", SizeGiB: 6}, {Id: "new", SizeGiB: 5}}}
	for _, test := range []struct {
		restoring bool
		want      diskBudget
	}{{false, diskBudget{system: 3, data: 11}}, {true, diskBudget{system: 5, data: 15}}} {
		if actual := peakDiskBudget(before, after, test.restoring); actual != test.want {
			t.Fatalf("restoring=%v peak=%+v want=%+v", test.restoring, actual, test.want)
		}
		previous := []Target{{Execution: api.AssetExecution{InstanceId: "instance", Asset: before}}}
		targets := []Target{{Execution: api.AssetExecution{InstanceId: "instance", Asset: after}}}
		raw, err := resourceRecords("environment", targets, previous, test.restoring)
		var records []reservation
		if err != nil || json.Unmarshal(raw, &records) != nil || len(records) != 1 {
			t.Fatal(string(raw), err)
		}
		if records[0].SystemGiB != test.want.system || records[0].DiskGiB != test.want.system+test.want.data || !reflect.DeepEqual(records[0].Execution.Asset, after) {
			t.Fatalf("reservation changed the real execution: %+v", records[0])
		}
	}
	removed := after
	removed.Volumes = &[]api.Volume{{Id: "replacement", SizeGiB: 4}}
	if actual := peakDiskBudget(before, removed, false); actual != (diskBudget{system: 3, data: 8}) {
		t.Fatalf("removed and new volume budgets were released before cleanup: %+v", actual)
	}
}

func TestStorageBudgetFollowsFilesystems(t *testing.T) {
	root := api.StorageInfo{Filesystem: "root", CapacityBytes: 10 << 30, AvailableBytes: 10 << 30}
	pool := storageCandidate{id: "pool", node: "node", info: api.StorageInfo{Filesystem: "data", CapacityBytes: 100 << 30, AvailableBytes: 100 << 30}}
	storage := map[string]storageCandidate{storageKey("node", defaultStorage("node")): {node: "node", container: &root}, storageKey("node", "pool"): pool}
	amounts, err := storageAmounts("node", api.Container, pool, diskBudget{system: 8, data: 3}, storage)
	if err != nil || !reflect.DeepEqual(amounts, map[string]int64{"node/root": 8, "node/data": 3}) {
		t.Fatal(amounts, err)
	}
	limits := storageLimits(storage)
	if !storageFits(nil, nil, limits, amounts) || storageFits(map[string]int64{"node/root": 3}, nil, limits, amounts) {
		t.Fatal("container root budget was admitted against its data pool")
	}
	root.Filesystem, root.CapacityBytes, root.AvailableBytes = "data", 10<<30, 10<<30
	amounts, err = storageAmounts("node", api.Container, pool, diskBudget{system: 8, data: 3}, storage)
	if err != nil || amounts["node/data"] != 11 || storageFits(nil, nil, storageLimits(storage), amounts) {
		t.Fatal("same-filesystem components were not combined", amounts, err)
	}
	limits = map[string]api.StorageInfo{"disk": {CapacityBytes: 100 << 30, AvailableBytes: 20 << 30}}
	if !storageFits(map[string]int64{"disk": 80}, nil, limits, map[string]int64{"disk": 20}) {
		t.Fatal("existing reservations were subtracted from available space twice")
	}
	if storageFits(map[string]int64{"disk": 80}, map[string]int64{"disk": 15}, limits, map[string]int64{"disk": 10}) {
		t.Fatal("one batch admitted more new physical demand than available space")
	}
}
