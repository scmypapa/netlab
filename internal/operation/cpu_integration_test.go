package operation

import (
	"context"
	"encoding/json"
	"testing"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
)

func TestFractionalCPUReservations(t *testing.T) {
	pool := concurrencyPool(t, 4)
	ctx := context.Background()
	q := queries.New(pool)
	if err := q.PutNode(ctx, queries.PutNodeParams{ID: "cpu", Name: "cpu", Endpoint: "https://cpu.test", Info: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateEnvironment(ctx, queries.CreateEnvironmentParams{ID: "cpu", ProjectID: "default", Name: "CPU", Spec: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	targets := []Target{}
	for _, id := range []string{"one", "two", "three"} {
		targets = append(targets, Target{NodeID: "cpu", Execution: api.AssetExecution{InstanceId: id, Asset: api.Asset{Id: id, Resources: api.Resources{Cpu: 0.1, MemoryMiB: 64, DiskGiB: 1}}}})
	}
	raw, _ := resourceRecords("cpu", targets)
	if err := q.ReserveAssets(ctx, raw); err != nil {
		t.Fatal(err)
	}
	assertCPU := func(want float64) {
		t.Helper()
		reserved, err := q.GetReservedResources(ctx, "cpu")
		if err != nil || reserved.Cpu != want {
			t.Fatal("fractional CPU reservation", reserved, err)
		}
	}
	assertCPU(0.3)
	targets[0].Execution.Asset.Resources.Cpu = 0.25
	raw, _ = resourceRecords("cpu", targets[:1])
	if err := q.ReserveResourceUpdates(ctx, raw); err != nil {
		t.Fatal(err)
	}
	assertCPU(0.45)
	if err := q.UpdateExecutions(ctx, raw); err != nil {
		t.Fatal(err)
	}
	assertCPU(0.45)
	assets, err := q.ListRuntimeAssets(ctx, "cpu")
	if err != nil || len(assets) != 3 {
		t.Fatal("updated asset inventory", assets, err)
	}
	var execution api.AssetExecution
	if err = json.Unmarshal(assets[0].Execution, &execution); err != nil || assets[0].Cpu != 0.25 || execution.Asset.Resources.Cpu != 0.25 {
		t.Fatal("updated execution and CPU lost", assets[0], err)
	}
	if err := q.ReleaseAssets(ctx, []string{"one", "two", "three"}); err != nil {
		t.Fatal(err)
	}
	assertCPU(0)
}
