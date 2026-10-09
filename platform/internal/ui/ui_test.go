package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUIServing(t *testing.T) {
	h := Handler()
	get := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}
	r := get(http.MethodGet, "/")
	if r.Code != 200 || !strings.Contains(r.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("index: %d %v", r.Code, r.Header())
	}
	csp := r.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'", "base-uri 'none'", "object-src 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q: %s", want, csp)
		}
	}
	if r.Header().Get("X-Content-Type-Options") != "nosniff" || r.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("headers: %v", r.Header())
	}
	if !Built() {
		t.Skip("UI not built into this binary (scripts/build-web.sh): only the placeholder is served")
	}
	// Client-side routes get the app; missing assets are 404, not the app.
	if r := get(http.MethodGet, "/deployments/default/x"); r.Code != 200 || !strings.Contains(r.Body.String(), `<div id="root">`) {
		t.Fatalf("SPA fallback: %d", r.Code)
	}
	if r := get(http.MethodGet, "/assets/missing.js"); r.Code != 404 {
		t.Fatalf("missing asset: %d", r.Code)
	}
	if r := get(http.MethodPost, "/"); r.Code != 405 {
		t.Fatalf("POST: %d", r.Code)
	}
}
