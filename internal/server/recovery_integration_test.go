package server

import (
	"context"
	"encoding/json"
	"testing"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/operation"
)

func testRecoveryAPI(t *testing.T, ctx context.Context, s *Server, admin string, call func(string, string, string, any, int) []byte) {
	t.Run("recovery requests use scoped permissions and the existing operation", func(t *testing.T) {
		identity, err := s.Access.Authenticate(ctx, admin)
		if err != nil {
			t.Fatal(err)
		}
		spec := api.EnvironmentSpec{Assets: []api.Asset{}, Networks: []api.Network{}}
		env, err := s.Environments.Create(ctx, identity, api.CreateEnvironment{Name: "Recovery contract", Spec: &spec})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(spec)
		if err = s.Queries.CommitEnvironment(ctx, queries.CommitEnvironmentParams{ID: env.Id, AppliedSpec: raw, Status: "stopped"}); err != nil {
			t.Fatal(err)
		}
		var token api.IssuedServiceToken
		if err = json.Unmarshal(call("POST", "/service-tokens", admin, api.CreateServiceToken{Name: "Recovery reader", Grants: []api.ScopeGrant{{ScopeKind: api.ScopeGrantScopeKindEnvironment, ScopeId: env.Id, Permissions: []api.Permission{api.PermissionRead, api.PermissionOperate}}}}, 201), &token); err != nil {
			t.Fatal(err)
		}
		path := "/environments/" + env.Id + "/recovery-points"
		call("POST", path, token.Token, api.CaptureRecoveryPoint{Name: "Denied", ExpectedRevision: 1}, 403)
		call("POST", path, admin, api.CaptureRecoveryPoint{Name: "Stale", ExpectedRevision: 0}, 409)
		var point api.RecoveryPointSummary
		if err = json.Unmarshal(call("POST", path, admin, api.CaptureRecoveryPoint{Name: "Before change", ExpectedRevision: 1}, 201), &point); err != nil {
			t.Fatal(err)
		}
		call("POST", path, admin, api.CaptureRecoveryPoint{Name: "Concurrent", ExpectedRevision: 1}, 400)
		call("DELETE", path+"/"+point.Id, token.Token, nil, 403)
		var points []api.RecoveryPointSummary
		if err = json.Unmarshal(call("GET", path, token.Token, nil, 200), &points); err != nil || len(points) != 1 || points[0].State != api.RecoveryPointSummaryStateCapturing {
			t.Fatalf("point list: %v %v", points, err)
		}
		row, err := s.Queries.GetOperation(ctx, *point.OperationId)
		if err != nil {
			t.Fatal(err)
		}
		var payload operation.Payload
		if err = json.Unmarshal(row.Payload, &payload); err != nil || payload.Recovery == nil || payload.Recovery.ID != point.Id || row.ScopeID != env.Id || row.Kind != "capture-recovery" {
			t.Fatalf("capture queue identity: %+v %v", row, err)
		}
		if _, err = s.Pool.Exec(ctx, "UPDATE operations SET state='failed' WHERE id=$1", row.ID); err != nil {
			t.Fatal(err)
		}
		call("POST", "/operations/"+row.ID+"/retry", token.Token, nil, 403)
		call("POST", "/operations/"+row.ID+"/retry", admin, nil, 202)
		row, err = s.Queries.GetOperation(ctx, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(row.Payload, &payload); err != nil || payload.Recovery == nil || payload.Recovery.ID != point.Id {
			t.Fatalf("retry lost recovery identity: %v", err)
		}
		if _, err = s.Pool.Exec(ctx, "UPDATE operations SET state='succeeded',phase='complete' WHERE id=$1", row.ID); err != nil {
			t.Fatal(err)
		}
		captured, _ := json.Marshal(payload.Recovery)
		if err = s.Queries.CompleteRecoveryPoint(ctx, queries.CompleteRecoveryPointParams{ID: point.Id, State: "ready", Definition: captured}); err != nil {
			t.Fatal(err)
		}
		if err = s.Queries.SetEnvironmentState(ctx, queries.SetEnvironmentStateParams{ID: env.Id, Status: "stopped"}); err != nil {
			t.Fatal(err)
		}
		requestId := "clone-request"
		input := api.CreateEnvironment{Name: "Independent copy", RecoveryPointId: &point.Id, ClientRequestId: &requestId}
		call("POST", "/environments", token.Token, input, 403)
		var clone, replay api.Environment
		if err = json.Unmarshal(call("POST", "/environments", admin, input, 201), &clone); err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(call("POST", "/environments", admin, input, 201), &replay); err != nil {
			t.Fatal(err)
		}
		if clone.Id == env.Id || clone.Id != replay.Id || clone.OperationId == nil || clone.Status != api.EnvironmentStatusDeploying {
			t.Fatalf("clone identity: %+v %+v", clone, replay)
		}
		cloned, err := s.Queries.GetEnvironment(ctx, clone.Id)
		if err != nil || cloned.Status != string(clone.Status) {
			t.Fatalf("accepted clone state differs: %v", err)
		}
		cloneOperation, err := s.Queries.GetOperation(ctx, *clone.OperationId)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(cloneOperation.Payload, &payload); err != nil || cloneOperation.Kind != "clone-recovery" || payload.Run || payload.Recovery.EnvironmentID != env.Id || payload.Recovery.ID != point.Id {
			t.Fatalf("clone source lost: %v", err)
		}
		call("DELETE", path+"/"+point.Id, admin, nil, 409)
		if _, err = s.Pool.Exec(ctx, "UPDATE operations SET state='failed',phase='rolled-back' WHERE id=$1", cloneOperation.ID); err != nil {
			t.Fatal(err)
		}
		call("POST", "/operations/"+cloneOperation.ID+"/retry", admin, nil, 202)
		row, err = s.Queries.GetOperation(ctx, cloneOperation.ID)
		if err != nil || row.Phase != "queued" {
			t.Fatalf("clone retry: %+v %v", row, err)
		}
		if _, err = s.Pool.Exec(ctx, "UPDATE operations SET state='failed',phase='rollback' WHERE id=$1", cloneOperation.ID); err != nil {
			t.Fatal(err)
		}
		call("DELETE", path+"/"+point.Id, admin, nil, 409)
		if _, err = s.Pool.Exec(ctx, "UPDATE operations SET state='failed',phase='rolled-back' WHERE id=$1", cloneOperation.ID); err != nil {
			t.Fatal(err)
		}
		call("DELETE", path+"/"+point.Id, admin, nil, 202)
	})
}
