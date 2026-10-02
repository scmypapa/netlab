package operation

import (
	"testing"

	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
)

func TestRetryableUsesCurrentTaskAndTargetPermission(t *testing.T) {
	environmentID, operationID, ownerID, assetID := "environment", "operation", "owner", "asset"
	op := queries.Operation{ID: operationID, EnvironmentID: &environmentID, AssetID: &assetID, Kind: "rebuild", State: "failed"}
	env := queries.Environment{ID: environmentID, ProjectID: "project", OwnerID: &ownerID, OperationID: &operationID}
	user := access.Identity{Principal: queries.Principal{ID: "user", Kind: "user"}}
	grant := queries.Grant{ScopeKind: "asset", ScopeID: environmentID + "/" + assetID, Permissions: []string{"manage"}}
	user.Grants = []queries.Grant{grant}
	cases := []struct {
		name     string
		change   func(*access.Identity, *queries.Operation, *queries.Environment)
		expected bool
	}{
		{"asset manager", func(*access.Identity, *queries.Operation, *queries.Environment) {}, true},
		{"partial application", func(_ *access.Identity, op *queries.Operation, _ *queries.Environment) {
			op.State = "partially_applied"
		}, true},
		{"completed task", func(_ *access.Identity, op *queries.Operation, _ *queries.Environment) { op.State = "succeeded" }, false},
		{"older task", func(_ *access.Identity, _ *queries.Operation, env *queries.Environment) {
			current := "new-task"
			env.OperationID = &current
		}, false},
		{"operate cannot rebuild", func(identity *access.Identity, _ *queries.Operation, _ *queries.Environment) {
			identity.Grants[0].Permissions = []string{"operate"}
		}, false},
		{"other asset", func(identity *access.Identity, _ *queries.Operation, _ *queries.Environment) {
			identity.Grants[0].ScopeID = environmentID + "/other"
		}, false},
		{"environment owner", func(identity *access.Identity, _ *queries.Operation, _ *queries.Environment) {
			identity.Principal.ID = ownerID
			identity.Grants = nil
		}, true},
		{"token cannot inherit ownership", func(identity *access.Identity, _ *queries.Operation, _ *queries.Environment) {
			identity.Principal.ID = ownerID
			identity.Principal.Kind = "token"
			identity.Grants = nil
		}, false},
		{"change needs compose", func(_ *access.Identity, op *queries.Operation, _ *queries.Environment) { op.Kind = "change" }, false},
		{"node task needs administrator", func(_ *access.Identity, op *queries.Operation, _ *queries.Environment) { op.EnvironmentID = nil }, false},
		{"node task administrator", func(identity *access.Identity, op *queries.Operation, _ *queries.Environment) {
			op.EnvironmentID = nil
			identity.Principal.Administrator = true
		}, true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			identity, task, environment := user, op, env
			identity.Grants = append([]queries.Grant(nil), user.Grants...)
			tt.change(&identity, &task, &environment)
			if result := Retryable(identity, task, environment); result != tt.expected {
				t.Fatalf("retryable=%v, want %v", result, tt.expected)
			}
		})
	}
}
