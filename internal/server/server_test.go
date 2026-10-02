package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/environment"
)

func TestProductRoutesRequireAuthentication(t *testing.T) {
	s := (&Server{}).Handler()
	for _, route := range []string{"GET /api/v1/identity", "POST /api/v1/sessions/logout", "GET /api/v1/environments", "POST /api/v1/environments", "GET /api/v1/environments/env", "GET /api/v1/environments/env/state", "GET /api/v1/environments/env/events", "POST /api/v1/environments/env/actions", "POST /api/v1/environments/env/assets/asset/actions", "POST /api/v1/environments/env/changes", "PUT /api/v1/environments/env/view", "PUT /api/v1/environments/env/draft", "DELETE /api/v1/environments/env/draft", "GET /api/v1/operations", "GET /api/v1/operations/op", "GET /api/v1/templates", "POST /api/v1/templates", "GET /api/v1/nodes", "POST /api/v1/nodes"} {
		t.Run(route, func(t *testing.T) {
			method, path, _ := strings.Cut(route, " ")
			recorder := httptest.NewRecorder()
			s.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestHTTPErrorClasses(t *testing.T) {
	for _, item := range []struct {
		name   string
		err    error
		status int
	}{
		{"input", environment.Invalid("无效网段"), 400},
		{"unauthorized", access.ErrUnauthorized, 401},
		{"forbidden", access.ErrForbidden, 403},
		{"missing", pgx.ErrNoRows, 404},
		{"revision", environment.ErrConflict, 409},
		{"duplicate", &pgconn.PgError{Code: "23505"}, 409},
		{"database unavailable", errors.New("database unavailable"), 500},
		{"deadline", context.DeadlineExceeded, 504},
	} {
		t.Run(item.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeError(w, httptest.NewRequest("POST", "/api/v1/environments", nil), item.err)
			var problem api.Problem
			if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
				t.Fatal(err)
			}
			if w.Code != item.status || problem.Status != item.status {
				t.Fatalf("status=%d problem=%+v", w.Code, problem)
			}
		})
	}
}

func TestInvalidLoginAndCrossSiteRequests(t *testing.T) {
	s := (&Server{}).Handler()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/sessions/login", strings.NewReader("{")))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed login status=%d", w.Code)
	}
	w = httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/v1/sessions/login", strings.NewReader("{}"))
	r.Header.Set("Origin", "https://other.test")
	s.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-site status=%d", w.Code)
	}
}

func TestInfrastructureWritesRejectEnvironmentUsersAndPrivilegedTokens(t *testing.T) {
	s := &Server{}
	for _, identity := range []access.Identity{{Principal: queries.Principal{Kind: "user"}}, {Principal: queries.Principal{Kind: "token", Administrator: true}}} {
		for _, handler := range []endpoint{s.createTemplate, s.registerNode, s.listNodes} {
			if err := handler(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil), identity); !errors.Is(err, access.ErrForbidden) {
				t.Fatalf("expected forbidden, got %v", err)
			}
		}
	}
}
