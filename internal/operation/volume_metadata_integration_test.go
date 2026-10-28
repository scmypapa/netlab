package operation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/transport"
)

func TestContainerVolumeDataUpgrade(t *testing.T) {
	pool := concurrencyPool(t, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	q := queries.New(pool)
	var unavailable atomic.Bool
	var calls atomic.Int32
	unavailable.Store(true)
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if unavailable.Load() {
			http.Error(w, "archive unavailable", http.StatusServiceUnavailable)
			return
		}
		var request api.NodeTemplatePreparation
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		switch request.Template.Version {
		case 1:
			request.Template.ImageVolumes = &[]string{"/cache", "/data"}
		case 2:
			request.Template.ImageVolumes = &[]string{"/logs"}
		default:
			t.Errorf("requested archived-only template version %d", request.Template.Version)
		}
		json.NewEncoder(w).Encode(request.Template)
	}))
	defer node.Close()
	client := &transport.Client{HTTP: node.Client()}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	raw := func(value any) []byte {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if err := q.PutNode(ctx, queries.PutNodeParams{ID: "node", Name: "Node", Endpoint: node.URL, Info: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	owner := "node"
	old := api.Template{Id: "image", Name: "Image", Kind: api.Container, Version: 1, ArtifactNodeId: &owner}
	latest := old
	latest.Version = 2
	if err := q.CreateTemplate(ctx, queries.CreateTemplateParams{ID: latest.Id, Definition: raw(latest)}); err != nil {
		t.Fatal(err)
	}
	asset := api.Asset{Id: "server", Name: "Server", TemplateId: old.Id, Resources: api.Resources{Cpu: 1, MemoryMiB: 128, DiskGiB: 2}}
	spec := api.EnvironmentSpec{Assets: []api.Asset{asset}, Networks: []api.Network{}}
	if _, err := q.CreateEnvironment(ctx, queries.CreateEnvironmentParams{ID: "env", ProjectID: "default", Name: "Volume upgrade", Spec: raw(spec)}); err != nil {
		t.Fatal(err)
	}
	execution := api.AssetExecution{InstanceId: "instance", Asset: asset, Template: old}
	target := Target{NodeID: owner, State: "running", Execution: execution}
	recovery := Recovery{ID: "point", EnvironmentID: "env", Spec: spec, Assets: []Target{target}}
	payload := Payload{Spec: spec, BeforeSpec: &spec, Before: []Target{target}, Updates: []Target{target}, Recovery: &recovery}
	if _, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: "restore", EnvironmentID: &recovery.EnvironmentID, ScopeKind: "environment", ScopeID: "env", Kind: "restore-recovery", Payload: raw(payload)}); err != nil {
		t.Fatal(err)
	}
	draft := api.Draft{Spec: api.EnvironmentSpec{Assets: []api.Asset{asset}}}
	draft.Spec.Assets[0].Volumes = &[]api.Volume{{Id: "chosen", MountPath: "/logs", SizeGiB: 9}}
	exec("UPDATE operations SET state='running',phase='prepare' WHERE id='restore'")
	exec("UPDATE environments SET operation_id='restore',applied_spec=$1,draft=$2 WHERE id='env'", raw(spec), raw(draft))
	exec(`INSERT INTO runtime_assets(environment_id,asset_id,instance_id,node_id,execution,cpu,memory_mib,disk_gib,system_gib,current)
 VALUES('env','server','instance','node',$1,1,128,4,4,true)`, raw(execution))
	exec(`INSERT INTO recovery_points(id,environment_id,name,revision,definition,asset_count,operation_id)
 VALUES('point','env','Point',1,$1,1,'restore')`, raw(recovery))
	exec(`INSERT INTO backup_repositories(id,node_id,name,location,credentials) VALUES('repo','node','Repo','/archive','')`)
	archived := recovery
	archived.Assets = append([]Target(nil), recovery.Assets...)
	archived.Assets[0].Execution.Template.Version = 3
	exec(`INSERT INTO backups(id,environment_id,repository_id,name,definition,operation_id)
 VALUES('backup','env','repo','Backup',$1,'restore')`, raw(BackupDefinition{Recovery: archived}))
	if err := UpgradeContainerVolumes(ctx, pool, client); err == nil {
		t.Fatal("metadata failure was accepted")
	}
	var complete bool
	if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)", ContainerVolumeMigration).Scan(&complete); err != nil || complete {
		t.Fatalf("failed migration committed a completion marker: %v %v", complete, err)
	}
	var unchanged []byte
	if err := pool.QueryRow(ctx, "SELECT definition FROM templates WHERE id='image'").Scan(&unchanged); err != nil {
		t.Fatal(err)
	}
	var template api.Template
	if err := json.Unmarshal(unchanged, &template); err != nil || template.ImageVolumes != nil {
		t.Fatalf("metadata failure partially rewrote catalogue: %+v %v", template, err)
	}
	unavailable.Store(false)
	if err := UpgradeContainerVolumes(ctx, pool, client); err != nil {
		t.Fatal(err)
	}
	rows, err := q.ListRuntimeAssets(ctx, "env")
	if err != nil || len(rows) != 1 {
		t.Fatalf("runtime rows: %+v %v", rows, err)
	}
	if rows[0].SystemGib != 4 || rows[0].DiskGib != 8 || rows[0].MemoryMib != 128 || rows[0].Cpu != 1 {
		t.Fatalf("upgrade changed reservation: system=%d total=%d memory=%d cpu=%v", rows[0].SystemGib, rows[0].DiskGib, rows[0].MemoryMib, rows[0].Cpu)
	}
	if err := json.Unmarshal(rows[0].Execution, &execution); err != nil {
		t.Fatal(err)
	}
	wantOld := []api.Volume{{Id: "image-volume-0", MountPath: "/cache", SizeGiB: 1}, {Id: "image-volume-1", MountPath: "/data", SizeGiB: 1}}
	if execution.Asset.Resources.DiskGiB != 2 || execution.Asset.Volumes == nil || !reflect.DeepEqual(*execution.Asset.Volumes, wantOld) {
		t.Fatalf("upgrade changed the guest disk or lost its stable volumes: %+v", execution.Asset)
	}
	for _, test := range []struct {
		sql  string
		want []api.Volume
	}{
		{"SELECT spec->'assets'->0->'volumes' FROM environments WHERE id='env'", []api.Volume{{Id: "image-volume-0", MountPath: "/logs", SizeGiB: 1}}},
		{"SELECT applied_spec->'assets'->0->'volumes' FROM environments WHERE id='env'", wantOld},
		{"SELECT draft->'spec'->'assets'->0->'volumes' FROM environments WHERE id='env'", *draft.Spec.Assets[0].Volumes},
		{"SELECT payload->'spec'->'assets'->0->'volumes' FROM operations WHERE id='restore'", wantOld},
		{"SELECT definition->'spec'->'assets'->0->'volumes' FROM recovery_points WHERE id='point'", wantOld},
	} {
		var data []byte
		if err := pool.QueryRow(ctx, test.sql).Scan(&data); err != nil {
			t.Fatal(err)
		}
		var volumes []api.Volume
		if err := json.Unmarshal(data, &volumes); err != nil || !reflect.DeepEqual(volumes, test.want) {
			t.Fatalf("%s: %s, want %+v, error %v", test.sql, data, test.want, err)
		}
	}
	before := calls.Load()
	if err := UpgradeContainerVolumes(ctx, pool, client); err != nil || calls.Load() != before {
		t.Fatalf("completed upgrade repeated external preparation: calls %d -> %d, %v", before, calls.Load(), err)
	}
}
