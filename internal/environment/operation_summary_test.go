package environment

import (
	"encoding/json"
	"fmt"
	"testing"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
)

func operationSummaryFixture(size int) queries.Operation {
	spec := api.EnvironmentSpec{Assets: make([]api.Asset, size)}
	targets := make([]map[string]any, size)
	for i := range size {
		id := fmt.Sprintf("asset-%d", i)
		asset := api.Asset{Id: id, Name: "Application server " + id, TemplateId: "linux", Resources: api.Resources{Cpu: 2, MemoryMiB: 2048, DiskGiB: 30}, Interfaces: []api.Interface{{Id: "lan", NetworkId: "network", Address: fmt.Sprintf("10.0.%d.%d", i/250, i%250+1), Mac: "02:00:00:00:00:01"}}}
		spec.Assets[i] = asset
		targets[i] = map[string]any{"nodeId": "node", "execution": api.AssetExecution{Asset: asset, InstanceId: "instance-" + id, Template: api.Template{Id: "linux", Name: "Linux", Kind: api.Vm, Resources: asset.Resources}}, "state": "running"}
	}
	raw, _ := json.Marshal(map[string]any{"spec": spec, "beforeSpec": spec, "targets": targets, "before": targets, "unchanged": targets})
	return queries.Operation{ID: "operation", Kind: "start", State: "running", Phase: "prepare", Payload: raw, Results: []byte(`[]`)}
}

func TestOperationSummary(t *testing.T) {
	row := operationSummaryFixture(3)
	row.Results = []byte(`[{"assetId":"asset-0","instanceId":"instance-asset-0","state":"running"},{"assetId":"asset-1","instanceId":"retired-asset-1","state":"stopped"},{"assetId":"asset-2","instanceId":"instance-asset-2","state":"failed","error":"disk full"}]`)
	for _, check := range []struct {
		state, phase string
		completed    int
	}{{"queued", "queued", 0}, {"running", "prepare", 0}, {"running", "cleanup", 1}, {"failed", "cleanup", 0}, {"succeeded", "complete", 3}} {
		t.Run(check.state+"/"+check.phase, func(t *testing.T) {
			row.State, row.Phase = check.state, check.phase
			got, err := Operation(row)
			if err != nil || got.Total != 3 || got.Completed != check.completed || got.Results == nil || len(*got.Results) != 3 {
				t.Fatalf("summary=%+v error=%v", got, err)
			}
		})
	}
}

func TestOperationSummaryKinds(t *testing.T) {
	for _, check := range []struct {
		name, kind, payload string
		total               int
	}{
		{"service-port-change", "change", `{"beforeSpec":{"assets":[],"services":[{"id":"http","targetPort":80}]},"spec":{"assets":[],"services":[{"id":"http","targetPort":8080}]}}`, 1},
		{"asset-change", "change", `{"beforeSpec":{"assets":[{"id":"web","name":"Old"}]},"spec":{"assets":[{"id":"web","name":"New"}]},"updates":[{"execution":{"asset":{"id":"web"},"instanceId":"live"}}],"old":[{"execution":{"asset":{"id":"web"},"instanceId":"retired"}}]}`, 1},
		{"recovery", "capture-recovery", `{"recovery":{"assets":[{"execution":{"asset":{"id":"web"},"instanceId":"live"}},{"execution":{"asset":{"id":"db"},"instanceId":"db"}}]}}`, 2},
		{"template", "prepare-template", `{"template":{"id":"linux"}}`, 1},
		{"volume", "create-volume", `{"volume":{"id":"data","sizeGiB":20}}`, 1},
	} {
		t.Run(check.name, func(t *testing.T) {
			result, err := Operation(queries.Operation{Kind: check.kind, State: "succeeded", Phase: "complete", Payload: []byte(check.payload), Results: []byte(`[]`)})
			if err != nil || result.Total != check.total || result.Completed != check.total {
				t.Fatalf("summary=%+v error=%v", result, err)
			}
		})
	}
}

func BenchmarkOperationSummary(b *testing.B) {
	for _, size := range []int{100, 1000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			row := operationSummaryFixture(size)
			b.ReportAllocs()
			b.SetBytes(int64(len(row.Payload)))
			for b.Loop() {
				result, err := Operation(row)
				if err != nil || result.Total != size {
					b.Fatalf("total=%d error=%v", result.Total, err)
				}
			}
		})
	}
}
