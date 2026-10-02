package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/environment"
	"netlab.local/core/internal/transport"
)

type Server struct {
	Pool         *pgxpool.Pool
	Queries      *queries.Queries
	Access       access.Service
	Environments environment.Service
	Nodes        *transport.Client
	Web          http.Handler
	consoleMu    sync.Mutex
	consoles     map[*websocket.Conn]consoleOwner
}

func New(pool *pgxpool.Pool, nodes *transport.Client, web http.Handler) *Server {
	q := queries.New(pool)
	return &Server{Pool: pool, Queries: q, Access: access.Service{Queries: q}, Environments: environment.Service{Pool: pool, Queries: q}, Nodes: nodes, Web: web}
}

type endpoint func(http.ResponseWriter, *http.Request, access.Identity) error
type httpError struct {
	status int
	detail string
}

func (e httpError) Error() string { return e.detail }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	routes := map[string]endpoint{
		"GET /api/v1/identity":                                    s.identity,
		"POST /api/v1/sessions/logout":                            s.logout,
		"GET /api/v1/environments":                                s.listEnvironments,
		"POST /api/v1/environments":                               s.createEnvironment,
		"GET /api/v1/environments/{id}":                           s.getEnvironment,
		"GET /api/v1/environments/{id}/state":                     s.environmentState,
		"POST /api/v1/environments/{id}/actions":                  s.environmentAction,
		"POST /api/v1/environments/{id}/assets/{assetId}/actions": s.environmentAction,
		"GET /api/v1/environments/{id}/assets/{assetId}/console":  s.assetConsole,
		"POST /api/v1/environments/{id}/changes":                  s.environmentChanges,
		"PUT /api/v1/environments/{id}/view":                      s.saveView,
		"PUT /api/v1/environments/{id}/draft":                     s.saveDraft,
		"DELETE /api/v1/environments/{id}/draft":                  s.discardDraft,
		"GET /api/v1/environments/{id}/events":                    s.events,
		"GET /api/v1/operations":                                  s.listOperations,
		"GET /api/v1/operations/{id}":                             s.getOperation,
		"POST /api/v1/operations/{id}/retry":                      s.retryOperation,
		"GET /api/v1/templates":                                   s.listTemplates,
		"POST /api/v1/templates":                                  s.createTemplate,
		"GET /api/v1/nodes":                                       s.listNodes,
		"POST /api/v1/nodes":                                      s.registerNode,
		"GET /api/v1/blueprints":                                  s.listBlueprints,
		"GET /api/v1/blueprints/{id}":                             s.getBlueprint,
		"GET /api/v1/blueprints/{id}/versions":                    s.listBlueprintVersions,
		"GET /api/v1/blueprint-versions/{id}":                     s.getBlueprintVersion,
		"POST /api/v1/environments/{id}/blueprints":               s.saveEnvironmentBlueprint,
		"POST /api/v1/blueprints/{id}/versions":                   s.saveBlueprintVersion,
	}
	for pattern, handler := range routes {
		mux.HandleFunc(pattern, s.authorize(handler))
	}
	mux.HandleFunc("POST /api/v1/sessions/login", func(w http.ResponseWriter, r *http.Request) {
		if err := s.login(w, r); err != nil {
			writeError(w, r, err)
		}
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, httpError{http.StatusNotFound, "接口不存在"})
	})
	if s.Web != nil {
		mux.Handle("/", s.Web)
	}
	protection := http.NewCrossOriginProtection()
	protection.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, httpError{http.StatusForbidden, "跨站请求被拒绝"})
	}))
	return protection.Handler(mux)
}

func credential(r *http.Request) string {
	if value, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return value
	}
	if cookie, err := r.Cookie("netlab_session"); err == nil {
		return cookie.Value
	}
	return ""
}

func (s *Server) authorize(handler endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := credential(r)
		if token == "" {
			writeError(w, r, access.ErrUnauthorized)
			return
		}
		identity, err := s.Access.Authenticate(r.Context(), token)
		if err == nil {
			err = handler(w, r, identity)
		}
		if err != nil {
			writeError(w, r, err)
		}
	}
}

func decode(w http.ResponseWriter, r *http.Request, value any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	if err := decoder.Decode(value); err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			return httpError{http.StatusRequestEntityTooLarge, "请求超过 16 MiB"}
		}
		return httpError{http.StatusBadRequest, fmt.Sprintf("请求 JSON 无效：%s", err)}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, err = w.Write(raw)
	if err != nil {
		slog.Error("HTTP response interrupted", "error", err)
	}
	return nil
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	detail := "服务端处理失败"
	var problem httpError
	var pg *pgconn.PgError
	var validation *environment.ValidationError
	switch {
	case errors.As(err, &problem):
		status, detail = problem.status, problem.detail
	case errors.Is(err, access.ErrUnauthorized):
		status, detail = http.StatusUnauthorized, err.Error()
	case errors.Is(err, access.ErrForbidden):
		status, detail = http.StatusForbidden, err.Error()
	case errors.Is(err, pgx.ErrNoRows):
		status, detail = http.StatusNotFound, "对象不存在"
	case errors.Is(err, environment.ErrConflict):
		status, detail = http.StatusConflict, err.Error()
	case errors.As(err, &validation):
		status, detail = http.StatusBadRequest, validation.Error()
	case errors.As(err, &pg) && pg.Code == "23505":
		status, detail = http.StatusConflict, "对象或请求已存在"
	case errors.Is(err, context.DeadlineExceeded):
		status, detail = http.StatusGatewayTimeout, "执行请求超时"
	}
	if status >= 500 {
		slog.ErrorContext(r.Context(), "HTTP request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	}
	if err := writeJSON(w, status, api.Problem{Status: status, Title: http.StatusText(status), Detail: detail}); err != nil {
		slog.ErrorContext(r.Context(), "HTTP response failed", "error", err)
	}
}

func pagination(r *http.Request) (string, int32, error) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 500 {
			return "", 0, httpError{http.StatusBadRequest, "limit 应在 1–500 之间"}
		}
		limit = value
	}
	return r.URL.Query().Get("cursor"), int32(limit), nil
}

func (s *Server) identity(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	return writeJSON(w, http.StatusOK, api.Identity{Id: identity.Principal.ID, Name: identity.Principal.Name, Administrator: identity.Administrator()})
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) error {
	var input api.Login
	if err := decode(w, r, &input); err != nil {
		return err
	}
	identity, token, err := s.Access.Login(r.Context(), input)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: "netlab_session", Value: token, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode, MaxAge: int((12 * time.Hour).Seconds())})
	return writeJSON(w, http.StatusOK, identity)
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := s.Access.Logout(r.Context(), credential(r)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: "netlab_session", Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func requireAdministrator(identity access.Identity) error {
	if !identity.Administrator() {
		return access.ErrForbidden
	}
	return nil
}

func StaticFiles(directory string) http.Handler {
	files := http.FileServer(http.Dir(directory))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if !strings.Contains(path, ".") {
			copy := r.Clone(r.Context())
			copy.URL = &url.URL{Path: "/"}
			r = copy
		}
		files.ServeHTTP(w, r)
	})
}
