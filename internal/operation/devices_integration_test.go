package operation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"netlab.local/core/api"
	"netlab.local/core/db"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/transport"
)

func TestConcurrentPCIScheduling(t *testing.T) {
	dsn := os.Getenv("NETLAB_DATABASE_URL")
	if dsn == "" {
		t.Skip("NETLAB_DATABASE_URL selects PostgreSQL")
	}
	ctx := context.Background()
	base, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	schema := pgx.Identifier{"pci_" + strings.ReplaceAll(uuid.NewString(), "-", "")}.Sanitize()
	if _, err = base.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer base.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	q := queries.New(pool)
	info := api.NodeInfo{Id: "node", Capacity: api.Resources{Cpu: 16, MemoryMiB: 16384, DiskGiB: 100}, Capabilities: []string{"vm"},
		Storage:    &api.StorageInfo{Path: "/pool", Filesystem: "disk", CapacityBytes: 100 << 30, AvailableBytes: 100 << 30},
		VmHardware: &api.VmHardware{CpuModes: []string{"host-model"}, NicModels: []string{"virtio"}, Machines: []api.VmMachine{{Name: "q35", MaxVcpus: 16, Firmware: []string{"bios"}, DiskBuses: []string{"virtio"}}}, PciGroups: []api.PciGroup{{Id: "group", Available: true, Devices: []string{"0000:03:00.0", "0000:03:00.1"}}}},
	}
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(info) }))
	defer node.Close()
	raw, _ := json.Marshal(info)
	if err = q.PutNode(ctx, queries.PutNodeParams{ID: info.Id, Name: info.Id, Endpoint: node.URL, Info: raw}); err != nil {
		t.Fatal(err)
	}
	state := api.TemplateStateReady
	image := api.Template{Id: "image", Kind: api.Vm, Version: 1, State: &state, Hardware: &api.Hardware{Machine: "q35", Firmware: api.Bios, DiskBus: api.HardwareDiskBusVirtio, NicModel: api.HardwareNicModelVirtio}}
	raw, _ = json.Marshal(image)
	if err = q.CreateTemplate(ctx, queries.CreateTemplateParams{ID: image.Id, Definition: raw}); err != nil {
		t.Fatal(err)
	}
	worker := Worker{Pool: pool, Queries: q, Client: &transport.Client{HTTP: node.Client()}}
	type candidate struct {
		op      queries.Operation
		payload Payload
	}
	candidates := []candidate{}
	owner := "pci-test"
	for range 2 {
		spec := api.EnvironmentSpec{Networks: []api.Network{}, Assets: []api.Asset{{Id: uuid.NewString(), Name: "VM", TemplateId: image.Id, Resources: api.Resources{Cpu: 1, MemoryMiB: 512, DiskGiB: 1}, PciBinding: &api.PciBinding{NodeId: info.Id, GroupIds: []string{"group"}}}}}
		raw, _ = json.Marshal(spec)
		env, err := q.CreateEnvironment(ctx, queries.CreateEnvironmentParams{ID: uuid.NewString(), ProjectID: "default", Name: "PCI", Spec: raw})
		if err != nil {
			t.Fatal(err)
		}
		op, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: uuid.NewString(), EnvironmentID: &env.ID, ScopeKind: "environment", ScopeID: env.ID, Kind: "change", Payload: []byte(`{}`), ExpectedRevision: env.Revision})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, "UPDATE operations SET state='running',lease_owner=$2 WHERE id=$1", op.ID, owner); err != nil {
			t.Fatal(err)
		}
		op.LeaseOwner = &owner
		candidates = append(candidates, candidate{op, Payload{Spec: spec}})
	}
	start, results := make(chan struct{}), make(chan error, 2)
	for i := range candidates {
		go func() { <-start; item := &candidates[i]; results <- worker.plan(ctx, &item.op, &item.payload) }()
	}
	close(start)
	succeeded := 0
	for range 2 {
		if err := <-results; err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatal("exclusive device assigned more than once", succeeded)
	}
	reservations, err := q.DeviceReservations(ctx, []string{info.Id})
	if err != nil || len(reservations) != 1 {
		t.Fatal(reservations, err)
	}
	if _, err = pool.Exec(ctx, "DELETE FROM runtime_assets WHERE environment_id=$1", reservations[0].EnvironmentID); err != nil {
		t.Fatal(err)
	}
	for i := range candidates {
		item := &candidates[i]
		if item.op.Phase == "prepare" {
			continue
		}
		item.payload = Payload{Spec: item.payload.Spec}
		if err = worker.plan(ctx, &item.op, &item.payload); err != nil {
			t.Fatal("cleanup did not release device", err)
		}
	}
}
