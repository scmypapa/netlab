package server

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
)

func testNotifiedEventStreams(t *testing.T, parent context.Context, s *Server, token string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	identity, err := s.Access.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	spec := api.EnvironmentSpec{Assets: []api.Asset{}, Networks: []api.Network{}}
	env, err := s.Environments.Create(ctx, identity, api.CreateEnvironment{Name: "SSE concurrency", Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	watch, err := s.AccessUpdates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- watch() }()
	defer func() { cancel(); <-stopped }()
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	scanners := []*bufio.Scanner{}
	for range 32 {
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/environments/"+env.Id+"/events", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("stream status=%d", response.StatusCode)
		}
		scanner := bufio.NewScanner(response.Body)
		if !scanner.Scan() || scanner.Text() != ": connected" {
			t.Fatal("missing stream handshake")
		}
		scanners = append(scanners, scanner)
	}
	before := s.Pool.Stat().AcquireCount()
	select {
	case <-time.After(1200 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if extra := s.Pool.Stat().AcquireCount() - before; extra != 0 {
		t.Fatalf("idle streams polled database %d times", extra)
	}
	cursor, err := s.Queries.AddEvent(ctx, queries.AddEventParams{EnvironmentID: &env.Id, Kind: "runtime.changed", Payload: []byte(`{"ready":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, len(scanners))
	for _, scanner := range scanners {
		go func() {
			id, kind := "", ""
			for scanner.Scan() {
				line := scanner.Text()
				if strings.HasPrefix(line, "id: ") {
					id = strings.TrimPrefix(line, "id: ")
				}
				if strings.HasPrefix(line, "event: ") {
					kind = strings.TrimPrefix(line, "event: ")
				}
				if strings.HasPrefix(line, "data: ") {
					if id != fmt.Sprint(cursor) || kind != "runtime.changed" || line != `data: {"ready": true}` {
						results <- fmt.Errorf("unexpected event %s / %s / %s", id, kind, line)
					} else {
						results <- nil
					}
					return
				}
			}
			results <- fmt.Errorf("stream ended: %v", scanner.Err())
		}()
	}
	for range scanners {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}
