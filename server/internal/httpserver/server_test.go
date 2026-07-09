package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthz(t *testing.T) {
	h := New(BuildInfo{Version: "test", Commit: "abc123"}, Deps{})

	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("GET /healthz status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	var body healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
	if body.Version != "test" || body.Commit != "abc123" {
		t.Errorf("build info = %q/%q, want test/abc123", body.Version, body.Commit)
	}
}

func TestClientIP(t *testing.T) {
	var gotRemoteAddr string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRemoteAddr = r.RemoteAddr
	})

	t.Run("trusts CF-Connecting-IP", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "10.0.0.5:54321" // Caddy's docker-network peer address
		req.Header.Set("CF-Connecting-IP", "203.0.113.7")
		clientIP(next).ServeHTTP(httptest.NewRecorder(), req)
		if gotRemoteAddr != "203.0.113.7" {
			t.Fatalf("RemoteAddr = %q, want 203.0.113.7", gotRemoteAddr)
		}
	})

	t.Run("falls back to the bare TCP peer with no proxy in front (local/dev)", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "192.0.2.9:12345"
		clientIP(next).ServeHTTP(httptest.NewRecorder(), req)
		if gotRemoteAddr != "192.0.2.9" {
			t.Fatalf("RemoteAddr = %q, want 192.0.2.9 (port stripped)", gotRemoteAddr)
		}
	})

	t.Run("never trusts the client-controlled X-Forwarded-For header", func(t *testing.T) {
		// This is the exact spoof the deprecated chi middleware.RealIP was
		// vulnerable to (GHSA-3fxj-6jh8-hvhx): an attacker sending an
		// arbitrary leftmost XFF value to bypass the per-IP login rate
		// limit (docs/08-security.md section 4).
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "192.0.2.9:12345"
		req.Header.Set("X-Forwarded-For", "1.2.3.4")
		clientIP(next).ServeHTTP(httptest.NewRecorder(), req)
		if gotRemoteAddr != "192.0.2.9" {
			t.Fatalf("RemoteAddr = %q, want 192.0.2.9 (X-Forwarded-For must be ignored)", gotRemoteAddr)
		}
	})
}

func TestUnknownRouteIs404(t *testing.T) {
	h := New(BuildInfo{}, Deps{})

	req := httptest.NewRequest("GET", "/nope", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 404 {
		t.Fatalf("GET /nope status = %d, want 404", rec.Code)
	}
}
