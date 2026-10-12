package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBodiesOverLimitAreRejected(t *testing.T) {
	bin, logPath := stubClaude(t)
	srv := NewServer(bin, "git", nil)

	big := `{"action":"remove","ids":["` + strings.Repeat("a", maxBodyBytes) + `"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/bulk", strings.NewReader(big))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bulk: status = %d, want 400", rec.Code)
	}

	bigSpawn := `{"cwd":"/x","prompt":"` + strings.Repeat("a", maxBodyBytes) + `"}`
	req = httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(bigSpawn))
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("spawn: status = %d, want 400", rec.Code)
	}

	if c := calls(t, logPath); len(c) != 0 {
		t.Fatalf("claude was invoked for an oversize body: %v", c)
	}
}
