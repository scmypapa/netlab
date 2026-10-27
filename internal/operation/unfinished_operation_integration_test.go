package operation

import (
	"context"
	"errors"
	"testing"
	"time"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/environment"
)

func TestUnfinishedEnvironmentOperationKeepsRetryOwnership(t *testing.T) {
	pool := concurrencyPool(t, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	q := queries.New(pool)
	s := environment.Service{Pool: pool, Queries: q}
	identity := access.Identity{Principal: queries.Principal{ID: "admin", Kind: "user", Administrator: true}}
	if err := q.CreatePrincipal(ctx, queries.CreatePrincipalParams{ID: "admin", Name: "Admin", Kind: "user", Administrator: true}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		kind      string
		phase     string
		state     string
		committed bool
		migration bool
		blocked   bool
	}{
		{"start", "rollback", "failed", false, false, true},
		{"migrate", "migration-rollback", "failed", false, true, true},
		{"start", "cleanup", "partially_applied", true, false, true},
		{"migrate", "migration-cleanup", "partially_applied", true, true, true},
		{"migrate", "migration-prepare", "failed", false, true, true},
		{"migrate", "migration-apply", "failed", false, true, true},
		{"move-network", "network-move-copy", "failed", false, false, true},
		{"start", "queued", "failed", false, false, false},
		{"migrate", "queued", "failed", false, false, false},
		{"move-network", "queued", "failed", false, false, false},
		{"start", "rolled-back", "failed", false, false, false},
		{"start", "control", "failed", false, false, false},
		{"start", "complete", "failed", true, false, false},
	} {
		t.Run(test.phase, func(t *testing.T) {
			spec := api.EnvironmentSpec{Assets: []api.Asset{}, Networks: []api.Network{}}
			env, err := s.Create(ctx, identity, api.CreateEnvironment{Name: test.phase, Spec: &spec})
			if err != nil {
				t.Fatal(err)
			}
			op, err := s.Action(ctx, identity, env.Id, "", api.ActionRequest{Action: api.ActionRequestActionStart})
			if err != nil {
				t.Fatal(err)
			}
			payload := []byte(`{}`)
			if test.migration {
				payload = []byte(`{"migration":{"target":{"nodeId":"target"}}}`)
			}
			if _, err = pool.Exec(ctx, "UPDATE operations SET kind=$2,state=$3,phase=$4,payload=payload||jsonb_build_object('committed',$5::boolean)||$6::jsonb WHERE id=$1", op.Id, test.kind, test.state, test.phase, test.committed, payload); err != nil {
				t.Fatal(err)
			}
			_, err = s.Action(ctx, identity, env.Id, "", api.ActionRequest{Action: api.ActionRequestActionStart})
			if test.blocked {
				if !errors.Is(err, environment.ErrBusy) {
					t.Fatalf("unfinished %s was superseded: %v", test.phase, err)
				}
				row, err := q.GetEnvironment(ctx, env.Id)
				if err != nil || row.OperationID == nil || *row.OperationID != op.Id {
					t.Fatalf("unfinished operation lost ownership: %+v, %v", row, err)
				}
				retried, err := (Service{Pool: pool, Queries: q}).Retry(ctx, identity, op.Id)
				if err != nil || retried.Id != op.Id || retried.Phase != test.phase || retried.State != api.OperationStateQueued {
					t.Fatalf("original retry cannot continue its phase: %+v, %v", retried, err)
				}
			} else if err != nil {
				t.Fatalf("settled failure blocked a new action: %v", err)
			}
		})
	}
}
