package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/blueprint"
	"netlab.local/core/internal/environment"
)

func testTemplateLifecycleAPI(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *Server, token string, call func(string, string, string, any, int) []byte) {
	identity, err := s.Access.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	template := func() api.Template {
		t.Helper()
		state := api.TemplateStateReady
		result := api.Template{Id: uuid.NewString(), Name: "Lifecycle", Kind: api.Container, Os: "Linux", Version: 1, Source: "example/image:latest", Resources: api.Resources{Cpu: 1, MemoryMiB: 64, DiskGiB: 1}, State: &state}
		raw, _ := json.Marshal(result)
		if err := s.Queries.CreateTemplate(ctx, queries.CreateTemplateParams{ID: result.Id, Definition: raw}); err != nil {
			t.Fatal(err)
		}
		return result
	}
	spec := func(template api.Template) api.EnvironmentSpec {
		return api.EnvironmentSpec{Networks: []api.Network{}, Assets: []api.Asset{{Id: uuid.NewString(), Name: "Asset", TemplateId: template.Id, Resources: template.Resources, Interfaces: []api.Interface{}}}}
	}
	create := func(spec api.EnvironmentSpec) api.Environment {
		t.Helper()
		result, err := s.Environments.Create(ctx, identity, api.CreateEnvironment{Name: "Lifecycle", Spec: &spec})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	blocked := func(template api.Template, reason string) {
		t.Helper()
		raw := call("DELETE", "/templates/"+template.Id, token, nil, 409)
		if !strings.Contains(string(raw), reason) {
			t.Fatalf("missing reference %q: %s", reason, raw)
		}
	}
	t.Run("template deletion covers saved drafts pending changes and runtime facts", func(t *testing.T) {
		saved, draft, queued, runtime := template(), template(), template(), template()
		e := create(spec(saved))
		blocked(saved, "环境")
		call("PUT", "/environments/"+e.Id+"/draft", token, api.Draft{BaseRevision: e.Revision, Spec: spec(draft)}, 204)
		blocked(draft, "环境")
		payload, _ := json.Marshal(map[string]any{"spec": spec(queued)})
		if _, err := s.Queries.CreateOperation(ctx, queries.CreateOperationParams{ID: uuid.NewString(), EnvironmentID: &e.Id, ScopeKind: "environment", ScopeID: e.Id, Kind: "change", Payload: payload}); err != nil {
			t.Fatal(err)
		}
		blocked(queued, "待执行任务")
		r := create(api.EnvironmentSpec{Networks: []api.Network{}, Assets: []api.Asset{}})
		execution, _ := json.Marshal(map[string]any{"template": runtime})
		if _, err := pool.Exec(ctx, `INSERT INTO runtime_assets(environment_id,asset_id,instance_id,node_id,execution,cpu,memory_mib,disk_gib) VALUES($1,'runtime',$2,'test-node',$3,1,64,1);`, r.Id, uuid.NewString(), execution); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE environments SET status='destroyed' WHERE id=$1`, r.Id); err != nil {
			t.Fatal(err)
		}
		blocked(runtime, "运行资产")
	})
	t.Run("blueprints retain references until their environments are destroyed", func(t *testing.T) {
		image := template()
		e := create(spec(image))
		var b api.Blueprint
		if err := json.Unmarshal(call("POST", "/environments/"+e.Id+"/blueprints", token, api.SaveBlueprint{Name: "Lifecycle blueprint", ExpectedRevision: e.Revision, Spec: e.Spec}, 201), &b); err != nil {
			t.Fatal(err)
		}
		var copied api.Environment
		if err := json.Unmarshal(call("POST", "/environments", token, api.CreateEnvironment{Name: "Blueprint copy", BlueprintVersionId: &b.LatestVersionId}, 201), &copied); err != nil {
			t.Fatal(err)
		}
		call("DELETE", "/blueprints/"+b.Id, token, nil, 409)
		if _, err := pool.Exec(ctx, `UPDATE environments SET status='destroyed' WHERE id=ANY($1::text[])`, []string{e.Id, copied.Id}); err != nil {
			t.Fatal(err)
		}
		blocked(image, "环境模板")
		call("DELETE", "/blueprints/"+b.Id, token, nil, 204)
		call("GET", "/blueprints/"+b.Id, token, nil, 404)
		call("DELETE", "/templates/"+image.Id, token, nil, 202)
		var refs int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM environments WHERE blueprint_version_id=$1`, b.LatestVersionId).Scan(&refs); err != nil || refs != 0 {
			t.Fatalf("blueprint provenance not detached: %d %v", refs, err)
		}
	})
	t.Run("deleted templates cannot be reintroduced by draft or preparation retry", func(t *testing.T) {
		image := template()
		e := create(api.EnvironmentSpec{Networks: []api.Network{}, Assets: []api.Asset{}})
		payload, _ := json.Marshal(map[string]any{"template": image})
		op, err := s.Queries.CreateOperation(ctx, queries.CreateOperationParams{ID: uuid.NewString(), ScopeKind: "template", ScopeID: image.Id, Kind: "prepare-template", Payload: payload})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE operations SET state='failed' WHERE id=$1`, op.ID); err != nil {
			t.Fatal(err)
		}
		call("DELETE", "/templates/"+image.Id, token, nil, 202)
		call("PUT", "/environments/"+e.Id+"/draft", token, api.Draft{Spec: spec(image)}, 400)
		call("POST", "/operations/"+op.ID+"/retry", token, nil, 400)
	})
	t.Run("creation and template deletion serialize across concurrent requests", func(t *testing.T) {
		for range 30 {
			image := template()
			createDone, deleteDone := make(chan error, 1), make(chan error, 1)
			go func() {
				input := spec(image)
				_, err := s.Environments.Create(ctx, identity, api.CreateEnvironment{Name: "Concurrent", Spec: &input})
				createDone <- err
			}()
			go func() {
				r := httptest.NewRequest("DELETE", "/api/v1/templates/"+image.Id, nil)
				r.SetPathValue("id", image.Id)
				deleteDone <- s.deleteTemplate(httptest.NewRecorder(), r, identity)
			}()
			createErr, deleteErr := <-createDone, <-deleteDone
			var conflict httpError
			var invalid *environment.ValidationError
			if !((createErr == nil && errors.As(deleteErr, &conflict) && conflict.status == 409) || (deleteErr == nil && errors.As(createErr, &invalid))) {
				t.Fatalf("creation=%v deletion=%v", createErr, deleteErr)
			}
			var inconsistent bool
			if err := pool.QueryRow(ctx, `SELECT definition->>'state'='deleting' AND EXISTS(SELECT 1 FROM environments WHERE spec @> jsonb_build_object('assets',jsonb_build_array(jsonb_build_object('templateId',$1::text)))) FROM templates WHERE id=$1`, image.Id).Scan(&inconsistent); err != nil || inconsistent {
				t.Fatalf("deleted template was referenced: %t %v", inconsistent, err)
			}
		}
	})
	t.Run("blueprint deletion and creation preserve the version reference atomically", func(t *testing.T) {
		service := blueprint.Service{Pool: pool, Queries: s.Queries}
		for range 15 {
			source := create(api.EnvironmentSpec{Networks: []api.Network{}, Assets: []api.Asset{}})
			b, version, err := service.Save(ctx, identity, source.Id, "", "Concurrent blueprint", source.Revision, source.Spec)
			if err != nil {
				t.Fatal(err)
			}
			createDone, deleteDone := make(chan error, 1), make(chan error, 1)
			go func() {
				_, err := s.Environments.Create(ctx, identity, api.CreateEnvironment{Name: "Concurrent copy", BlueprintVersionId: &version.Id})
				createDone <- err
			}()
			go func() { deleteDone <- service.Delete(ctx, identity, b.Id) }()
			createErr, deleteErr := <-createDone, <-deleteDone
			if !((createErr == nil && errors.Is(deleteErr, environment.ErrInUse)) || (deleteErr == nil && errors.Is(createErr, pgx.ErrNoRows))) {
				t.Fatalf("creation=%v deletion=%v", createErr, deleteErr)
			}
		}
	})
}
