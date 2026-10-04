package server

import (
	"errors"
	"net/http/httptest"
	"testing"

	"netlab.local/core/internal/access"
)

func TestUpdatesRequireAdministrator(t *testing.T) {
	s := &Server{}
	for _, handler := range []endpoint{s.systemUpdate, s.checkSystemUpdate, s.applySystemUpdate} {
		if err := handler(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil), access.Identity{}); !errors.Is(err, access.ErrForbidden) {
			t.Fatal("update endpoint accepted non-administrator", err)
		}
	}
}
