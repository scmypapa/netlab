package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"netlab.local/core/api"
	"strconv"
	"time"

	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
)

func (s *Server) events(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	id := r.PathValue("id")
	_, visible, err := s.Environments.Readable(r.Context(), identity, id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	r = r.WithContext(ctx)
	release, err := s.trackConnection(ctx, &accessConnection{
		principal: identity.Principal.ID, environment: id, credential: credential(r), permission: "read", close: cancel,
	})
	if err != nil {
		return err
	}
	defer release()
	var cursor int64
	if raw := r.Header.Get("Last-Event-ID"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 {
			return httpError{http.StatusBadRequest, "Last-Event-ID 无效"}
		}
		cursor = value
	} else if cursor, err = s.Queries.CurrentEventCursor(r.Context(), &id); err != nil {
		return err
	}
	rows, err := s.Queries.ReadEvents(r.Context(), queries.ReadEventsParams{EnvironmentID: &id, Cursor: cursor})
	if err != nil {
		return err
	}
	flusher := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
		return err
	}
	if err := flusher.Flush(); err != nil {
		return err
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		if len(rows) > 0 {
			identity, err = s.Access.Authenticate(r.Context(), credential(r))
			if err == nil {
				_, visible, err = s.Environments.Readable(r.Context(), identity, id)
			}
			if err != nil {
				return nil
			}
			for _, event := range rows {
				kind, payload := event.Kind, event.Payload
				if visible != nil {
					// Restricted readers receive invalidation only; /state applies
					// the same asset scope before returning runtime and operation data.
					kind, payload = "runtime.changed", []byte(`{}`)
				}
				if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Cursor, kind, payload); err != nil {
					return nil
				}
				cursor = event.Cursor
			}
			if err = flusher.Flush(); err != nil {
				return nil
			}
		}
		select {
		case <-r.Context().Done():
			return nil
		case <-ticker.C:
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return nil
			}
			if err := flusher.Flush(); err != nil {
				return nil
			}
		}
		rows, err = s.Queries.ReadEvents(r.Context(), queries.ReadEventsParams{EnvironmentID: &id, Cursor: cursor})
		if err != nil {
			slog.ErrorContext(r.Context(), "event stream failed", "environment", id, "error", err)
			problem, _ := json.Marshal(api.Problem{Status: 500, Title: "Event stream failed", Detail: "事件通道中断"})
			if _, err = fmt.Fprintf(w, "event: stream-error\ndata: %s\n\n", problem); err == nil {
				_ = flusher.Flush()
			}
			return nil
		}
	}
}
