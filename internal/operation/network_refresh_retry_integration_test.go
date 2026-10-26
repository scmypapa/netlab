package operation

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
)

func TestNetworkRefreshRetryPreservesBusinessIdentity(t *testing.T) {
	for _, businessState := range []string{"failed", "succeeded"} {
		t.Run(businessState, func(t *testing.T) {
			pool := concurrencyPool(t, 4)
			ctx := context.Background()
			q := queries.New(pool)
			s := Service{Pool: pool, Queries: q}
			env, err := q.CreateEnvironment(ctx, queries.CreateEnvironmentParams{
				ID: "environment", ProjectID: "default", Name: "Existing",
				Spec: []byte(`{"networks":[],"assets":[]}`),
			})
			if err != nil {
				t.Fatal(err)
			}
			business, err := q.CreateOperation(ctx, queries.CreateOperationParams{
				ID: "business", EnvironmentID: &env.ID, ScopeKind: "environment", ScopeID: env.ID, Kind: "change",
				Payload: []byte(`{"spec":{"networks":[],"assets":[]},"beforeSpec":{"networks":[],"assets":[]}}`),
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(ctx, "UPDATE operations SET state=$2,phase='cleanup' WHERE id=$1", business.ID, businessState); err != nil {
				t.Fatal(err)
			}
			var businessError *string
			status := "stopped"
			if businessState == "failed" {
				message := "retained business cleanup failure"
				businessError, status = &message, "running"
			}
			if _, err = pool.Exec(ctx, `UPDATE environments SET operation_id=$2,status=$3,error=$4,revision=8,applied_spec=spec WHERE id=$1`, env.ID, business.ID, status, businessError); err != nil {
				t.Fatal(err)
			}
			before, err := q.GetEnvironment(ctx, env.ID)
			if err != nil {
				t.Fatal(err)
			}
			unchanged := func(stage string) {
				t.Helper()
				after, err := q.GetEnvironment(ctx, env.ID)
				if err != nil || !reflect.DeepEqual(after, before) {
					t.Fatalf("%s changed the business environment: before=%+v after=%+v error=%v", stage, before, after, err)
				}
			}
			refresh, err := q.CreateOperation(ctx, queries.CreateOperationParams{
				ID: "refresh", EnvironmentID: &env.ID, ScopeKind: "environment", ScopeID: env.ID,
				Kind: "refresh-network", ExpectedRevision: 2,
				Payload: []byte(`{"spec":{"networks":[{"id":"stale","name":"Old network","cidr":"10.0.0.0/24"}],"assets":[]},"beforeStatus":"changing"}`),
			})
			if err != nil {
				t.Fatal(err)
			}
			owner := "refresh-worker"
			claim := func() queries.Operation {
				t.Helper()
				claimed, err := q.ClaimOperation(ctx, queries.ClaimOperationParams{LeaseOwner: &owner})
				if err != nil || claimed.ID != refresh.ID {
					t.Fatalf("claim: %+v, %v", claimed, err)
				}
				return queries.Operation(claimed)
			}
			claim()
			unchanged("claim")
			if _, err := q.CreateOperation(ctx, queries.CreateOperationParams{
				ID: "waiting", EnvironmentID: &env.ID, ScopeKind: "environment", ScopeID: env.ID,
				Kind: "stop", Payload: []byte(`{}`),
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := q.ClaimOperation(ctx, queries.ClaimOperationParams{LeaseOwner: &owner}); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("same-scope operation bypassed the running auxiliary task: %v", err)
			}
			if _, err := pool.Exec(ctx, "UPDATE operations SET state='succeeded' WHERE id='waiting'"); err != nil {
				t.Fatal(err)
			}
			message := "network temporarily unavailable"
			if count, err := q.FinishOperation(ctx, queries.FinishOperationParams{
				ID: refresh.ID, LeaseOwner: &owner, State: "failed", Phase: "services", Results: []byte(`[]`), Error: &message,
			}); err != nil || count != 1 {
				t.Fatalf("failure persistence: %d, %v", count, err)
			}
			unchanged("failure")
			operator := access.Identity{
				Principal: queries.Principal{ID: "network-operator", Kind: "user"},
				Grants:    []queries.Grant{{ScopeKind: "environment", ScopeID: env.ID, Permissions: []string{"network"}}},
			}
			forbidden := operator
			forbidden.Grants = []queries.Grant{{ScopeKind: "environment", ScopeID: env.ID, Permissions: []string{"operate"}}}
			if _, err := s.Retry(ctx, forbidden, refresh.ID); !errors.Is(err, access.ErrForbidden) {
				t.Fatalf("asset operation permission allowed network management: %v", err)
			}
			for range 2 {
				result, err := s.Retry(ctx, operator, refresh.ID)
				if err != nil || result.Id != refresh.ID || result.State != api.OperationStateQueued {
					t.Fatalf("retry: %+v, %v", result, err)
				}
			}
			queued, err := q.GetOperation(ctx, refresh.ID)
			if err != nil || queued.Phase != "queued" || string(queued.Payload) != "{}" || queued.ExpectedRevision != before.Revision {
				t.Fatalf("retry kept a stale maintenance snapshot: %+v, %v", queued, err)
			}
			unchanged("retry")
			claim()
			if result, err := s.Retry(ctx, operator, refresh.ID); err != nil || result.State != api.OperationStateRunning {
				t.Fatalf("running retry did not return the same task: %+v, %v", result, err)
			}
			if count, err := q.FinishOperation(ctx, queries.FinishOperationParams{
				ID: refresh.ID, LeaseOwner: &owner, State: "succeeded", Phase: "complete", Results: []byte(`[]`),
			}); err != nil || count != 1 {
				t.Fatalf("success persistence: %d, %v", count, err)
			}
			unchanged("completion")
			if businessState == "failed" {
				admin := access.Identity{Principal: queries.Principal{ID: "admin", Kind: "user", Administrator: true}}
				currentBusiness, err := q.GetOperation(ctx, business.ID)
				if err != nil || !Retryable(admin, currentBusiness, before) {
					t.Fatalf("maintenance removed the original business retry: %v", err)
				}
				if _, err := s.Retry(ctx, admin, business.ID); err != nil {
					t.Fatalf("original business task cannot resume: %v", err)
				}
			}
		})
	}
}
