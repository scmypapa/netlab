package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/environment"
)

func testPersistentVolumesAPI(t *testing.T, ctx context.Context, s *Server, admin, user, node string, image api.Template, call func(string, string, string, any, int) []byte) {
	input := api.CreateVolume{Name: "独立数据", Kind: image.Kind, StoragePoolId: "default:" + node, SizeGiB: 1}
	userIdentity, err := s.Access.Authenticate(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Queries.PutGrant(ctx, queries.PutGrantParams{PrincipalID: userIdentity.Principal.ID, ScopeKind: "project", ScopeID: "default", Permissions: []string{"compose"}}); err != nil {
		t.Fatal(err)
	}
	call("POST", "/volumes", user, input, 403)
	call("GET", "/volumes", user, nil, 403)
	var op api.Operation
	if err := json.Unmarshal(call("POST", "/volumes", admin, input, 202), &op); err != nil {
		t.Fatal(err)
	}
	if op.Total != 1 {
		t.Fatal("volume task should have one target")
	}
	accepted, err := s.Queries.GetOperation(ctx, op.Id)
	if err != nil {
		t.Fatal(err)
	}
	id := accepted.ScopeID
	// This fixture tests database ownership and API semantics; node execution is tested separately.
	if _, err = s.Pool.Exec(ctx, `UPDATE operations SET state='succeeded' WHERE id=$1;`, op.Id); err != nil {
		t.Fatal(err)
	}
	if err = s.Queries.FinishVolume(ctx, queries.FinishVolumeParams{ID: id, State: "ready", SizeGib: 1}); err != nil {
		t.Fatal(err)
	}
	call("GET", "/volumes/"+id, admin, nil, 200)
	call("GET", "/volumes/missing", admin, nil, 404)
	identity, err := s.Access.Authenticate(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	asset := api.Asset{Id: uuid.NewString(), Name: "Writer", TemplateId: image.Id, Resources: image.Resources, Interfaces: []api.Interface{}, Volumes: &[]api.Volume{{Id: "data", MountPath: "/data", SizeGiB: 5, PersistentVolumeId: &id}}}
	spec := api.EnvironmentSpec{Networks: []api.Network{}, Assets: []api.Asset{asset}}
	type outcome struct {
		environment api.Environment
		err         error
	}
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			// Separate decoded requests, as with real concurrent HTTP calls.
			raw, _ := json.Marshal(spec)
			var copy api.EnvironmentSpec
			json.Unmarshal(raw, &copy)
			run := true
			env, err := s.Environments.Create(ctx, identity, api.CreateEnvironment{Name: "Volume writer", Spec: &copy, Run: &run})
			results <- outcome{env, err}
		}()
	}
	var owner api.Environment
	acceptedCount := 0
	for range 2 {
		result := <-results
		if result.err == nil {
			owner = result.environment
			acceptedCount++
		} else if !errors.Is(result.err, environment.ErrInUse) {
			t.Fatal(result.err)
		}
	}
	if acceptedCount != 1 || (*owner.Spec.Assets[0].Volumes)[0].SizeGiB != 1 {
		t.Fatal("concurrent writer exclusion or catalogue capacity resolution failed")
	}
	refs, err := s.Queries.PersistentVolumeReferences(ctx, []string{id})
	if err != nil || len(refs) != 1 || refs[0].Name != "环境：Volume writer" {
		t.Fatalf("duplicate or missing usage: %+v %v", refs, err)
	}
	call("DELETE", "/volumes/"+id, admin, nil, 409)
	call("PUT", "/volumes/"+id, admin, map[string]int{"sizeGiB": 2}, 409)
	var blueprint api.Blueprint
	if err = json.Unmarshal(call("POST", "/environments/"+owner.Id+"/blueprints", admin, api.SaveBlueprint{Name: "New data", ExpectedRevision: owner.Revision, Spec: owner.Spec}, 201), &blueprint); err != nil {
		t.Fatal(err)
	}
	var version api.BlueprintVersion
	if err = json.Unmarshal(call("GET", "/blueprint-versions/"+blueprint.LatestVersionId, admin, nil, 200), &version); err != nil {
		t.Fatal(err)
	}
	if (*version.Spec.Assets[0].Volumes)[0].PersistentVolumeId != nil {
		t.Fatal("blueprint retained attachment to live data")
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE operations SET state='failed' WHERE id=$1;`, *owner.OperationId); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE environments SET status='destroyed' WHERE id=$1`, owner.Id); err != nil {
		t.Fatal(err)
	}
	var draftOwner api.Environment
	call("POST", "/environments", user, api.CreateEnvironment{Name: "Unauthorized attachment", Spec: &spec}, 403)
	if err = json.Unmarshal(call("POST", "/environments", user, api.CreateEnvironment{Name: "Draft owner", Spec: &api.EnvironmentSpec{Networks: []api.Network{}, Assets: []api.Asset{}}}, 201), &draftOwner); err != nil {
		t.Fatal(err)
	}
	call("PUT", "/environments/"+draftOwner.Id+"/draft", user, api.Draft{BaseRevision: 0, Spec: spec}, 403)
	call("PUT", "/environments/"+draftOwner.Id+"/draft", admin, api.Draft{BaseRevision: 0, Spec: spec}, 204)
	call("DELETE", "/volumes/"+id, admin, nil, 409)
	if _, err = s.Pool.Exec(ctx, `UPDATE environments SET draft=NULL WHERE id=$1`, draftOwner.Id); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(call("PUT", "/volumes/"+id, admin, map[string]int{"sizeGiB": 2}, 202), &op); err != nil {
		t.Fatal(err)
	}
	allocations, err := s.Queries.StorageReservations(ctx, []string{node})
	if err != nil || len(allocations) != 1 || allocations[0].DiskGib != 2 {
		t.Fatalf("pending growth not reserved: %+v %v", allocations, err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE operations SET state='failed' WHERE id=$1`, op.Id); err != nil {
		t.Fatal(err)
	}
	if err = s.Queries.SetVolumeOperation(ctx, queries.SetVolumeOperationParams{ID: id, State: "failed", OperationID: &op.Id}); err != nil {
		t.Fatal(err)
	}
	call("POST", "/operations/"+op.Id+"/retry", admin, nil, 202)
	if _, err = s.Pool.Exec(ctx, `UPDATE operations SET state='failed' WHERE id=$1`, op.Id); err != nil {
		t.Fatal(err)
	}
	if err = s.Queries.SetVolumeOperation(ctx, queries.SetVolumeOperationParams{ID: id, State: "failed", OperationID: &op.Id}); err != nil {
		t.Fatal(err)
	}
	call("DELETE", "/volumes/"+id, admin, nil, 202)
	call("POST", "/operations/"+op.Id+"/retry", admin, nil, 409)
}
