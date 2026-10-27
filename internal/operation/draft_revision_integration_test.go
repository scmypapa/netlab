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

func TestUnstartedEnvironmentChangesAdvanceRevision(t *testing.T) {
	pool := concurrencyPool(t, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	q := queries.New(pool)
	s := environment.Service{Pool: pool, Queries: q}
	identity := access.Identity{Principal: queries.Principal{ID: "admin", Kind: "user", Administrator: true}}
	if err := q.CreatePrincipal(ctx, queries.CreatePrincipalParams{ID: "admin", Name: "Admin", Kind: "user", Administrator: true}); err != nil {
		t.Fatal(err)
	}
	initial := api.EnvironmentSpec{Assets: []api.Asset{}, Networks: []api.Network{}}
	create := func() api.Environment {
		t.Helper()
		result, err := s.Create(ctx, identity, api.CreateEnvironment{Name: "Draft revision", Spec: &initial})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	change := func(id, name string, revision int) error {
		spec := initial
		spec.Networks = []api.Network{{Id: "lan", Name: name, Cidr: "10.1.0.0/24"}}
		_, op, err := s.Change(ctx, identity, id, api.ChangeRequest{Apply: true, ExpectedRevision: revision, Spec: spec})
		if op != nil {
			return errors.New("unstarted configuration change created a runtime operation")
		}
		return err
	}
	read := func(id string) api.Environment {
		t.Helper()
		row, err := q.GetEnvironment(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		result, err := environment.Record(row)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	t.Run("stale sequential write", func(t *testing.T) {
		env := create()
		if err := change(env.Id, "First", env.Revision); err != nil {
			t.Fatal(err)
		}
		updated := read(env.Id)
		if updated.Revision != env.Revision+1 {
			t.Fatalf("accepted configuration did not advance revision: %d -> %d", env.Revision, updated.Revision)
		}
		if err := change(env.Id, "Stale", env.Revision); !errors.Is(err, environment.ErrConflict) {
			t.Fatalf("stale configuration overwrote the accepted change: %v", err)
		}
		if actual := read(env.Id); actual.Spec.Networks[0].Name != "First" || actual.Revision != updated.Revision {
			t.Fatalf("stale request altered configuration: %+v", actual)
		}
		if err := change(env.Id, "Next", updated.Revision); err != nil {
			t.Fatal(err)
		}
		if actual := read(env.Id); actual.Revision != updated.Revision+1 || actual.Spec.Networks[0].Name != "Next" {
			t.Fatalf("fresh configuration did not commit: %+v", actual)
		}
	})

	t.Run("concurrent writers", func(t *testing.T) {
		env := create()
		type result struct {
			name string
			err  error
		}
		results := make(chan result, 2)
		for _, name := range []string{"First", "Second"} {
			go func() { results <- result{name, change(env.Id, name, env.Revision)} }()
		}
		winner := ""
		for range 2 {
			r := <-results
			if r.err == nil {
				if winner != "" {
					t.Fatal("both writers with the same revision were accepted")
				}
				winner = r.name
			} else if !errors.Is(r.err, environment.ErrConflict) {
				t.Fatal(r.err)
			}
		}
		actual := read(env.Id)
		if winner == "" || actual.Revision != env.Revision+1 || actual.Spec.Networks[0].Name != winner {
			t.Fatalf("winner=%s actual=%+v", winner, actual)
		}
	})
}
