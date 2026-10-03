package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/transport"
)

func testStoragePoolsAPI(t *testing.T, ctx context.Context, s *Server, admin string, call func(string, string, string, any, int) []byte) {
	id := uuid.NewString()
	cleaned := 0
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			info := api.StorageInfo{Path: r.URL.Query().Get("path"), Filesystem: "root", CapacityBytes: 8 << 30, AvailableBytes: 4 << 30}
			if r.URL.Path == "/node/v1/info" {
				info.Path = "/var/lib/netlab"
				writeJSON(w, 200, api.NodeInfo{Id: id, Name: "Storage node", Storage: &info})
			} else {
				writeJSON(w, 200, info)
			}
		case http.MethodPost:
			writeJSON(w, 200, api.StorageInfo{Path: "/mnt/pool/netlab-" + id + "/" + r.PathValue("id"), Filesystem: "root", CapacityBytes: 8 << 30, AvailableBytes: 4 << 30})
		case http.MethodDelete:
			cleaned++
			w.WriteHeader(204)
		}
	}))
	defer node.Close()
	s.Nodes = &transport.Client{HTTP: node.Client()}
	if err := s.Queries.PutNode(ctx, queries.PutNodeParams{ID: id, Name: "Storage node", Endpoint: node.URL, Info: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	var pool api.StoragePool
	if err := json.Unmarshal(call("POST", "/storage-pools", admin, api.CreateStoragePool{NodeId: id, Name: "Data", Directory: "/mnt/pool"}, 201), &pool); err != nil {
		t.Fatal(err)
	}
	call("POST", "/storage-pools", admin, api.CreateStoragePool{NodeId: id, Name: "Duplicate", Directory: "/mnt/pool"}, 409)
	if cleaned != 1 {
		t.Fatal("failed registration did not release its directory")
	}
	identity, err := s.Access.Authenticate(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	imageID := uuid.NewString()
	ready := api.TemplateStateReady
	image := api.Template{Id: imageID, Name: "Storage", Kind: api.Container, Version: 1, Resources: api.Resources{Cpu: 1, MemoryMiB: 64, DiskGiB: 1}, State: &ready}
	raw, _ := json.Marshal(image)
	if err = s.Queries.CreateTemplate(ctx, queries.CreateTemplateParams{ID: imageID, Definition: raw}); err != nil {
		t.Fatal(err)
	}
	volumes := []api.Volume{{Id: "data", MountPath: "/data", SizeGiB: 2}}
	a := api.Asset{Id: uuid.NewString(), Name: "Data", TemplateId: imageID, StoragePoolId: &pool.Id, Resources: image.Resources, Interfaces: []api.Interface{}, Volumes: &volumes}
	e, err := s.Environments.Create(ctx, identity, api.CreateEnvironment{Name: "Storage", Spec: &api.EnvironmentSpec{Assets: []api.Asset{a}, Networks: []api.Network{}}})
	if err != nil {
		t.Fatal(err)
	}
	if rejected := call("DELETE", "/storage-pools/"+pool.Id, admin, nil, 409); !strings.Contains(string(rejected), e.Name) {
		t.Fatal("reference name missing")
	}
	for range 2 {
		instance := uuid.NewString()
		exec, _ := json.Marshal(api.AssetExecution{Asset: a, InstanceId: instance, Template: image, StoragePoolId: &pool.Id})
		if _, err = s.Pool.Exec(ctx, "INSERT INTO runtime_assets(environment_id,asset_id,instance_id,node_id,execution,cpu,memory_mib,disk_gib) VALUES($1,$2,$3,$4,$5,1,64,3)", e.Id, a.Id, instance, id, exec); err != nil {
			t.Fatal(err)
		}
	}
	allocations, err := s.Queries.StorageReservations(ctx, []string{id})
	if err != nil || len(allocations) != 1 || allocations[0].DiskGib != 4 {
		t.Fatalf("shared volumes counted twice: %+v %v", allocations, err)
	}
	instances, err := s.Queries.ListRuntimeAssets(ctx, e.Id)
	if err != nil {
		t.Fatal(err)
	}
	pending, _ := json.Marshal([]map[string]any{{"instanceId": instances[0].InstanceID, "cpu": 1, "memoryMiB": 64, "diskGiB": 4}})
	if err = s.Queries.ReserveResourceUpdates(ctx, pending); err != nil {
		t.Fatal(err)
	}
	allocations, err = s.Queries.StorageReservations(ctx, []string{id})
	if err != nil || len(allocations) != 1 || allocations[0].DiskGib != 5 {
		t.Fatalf("pending disk growth was not reserved: %+v %v", allocations, err)
	}
	if _, err = s.Pool.Exec(ctx, "DELETE FROM runtime_assets WHERE environment_id=$1", e.Id); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, "UPDATE environments SET status='destroyed' WHERE id=$1", e.Id); err != nil {
		t.Fatal(err)
	}
	var op api.Operation
	if err = json.Unmarshal(call("DELETE", "/storage-pools/"+pool.Id, admin, nil, 202), &op); err != nil {
		t.Fatal(err)
	}
	call("DELETE", "/storage-pools/"+pool.Id, admin, nil, 409)
	var deleting queries.StoragePool
	if deleting, err = s.Queries.GetStoragePool(ctx, pool.Id); err != nil || deleting.State != "deleting" || deleting.OperationID == nil || *deleting.OperationID != op.Id {
		t.Fatalf("delete operation mismatch: %+v %v", deleting, err)
	}
	if _, err = s.Environments.Create(ctx, identity, api.CreateEnvironment{Name: "Deleting pool", Spec: &api.EnvironmentSpec{Assets: []api.Asset{a}, Networks: []api.Network{}}}); err == nil {
		t.Fatal("referenced deleting pool")
	}
	t.Log("storage reference, shared volume reservation and deletion checks passed:", pool.Name)
}
