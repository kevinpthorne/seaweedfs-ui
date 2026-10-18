package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"seaweedfs-ui/internal/config"
	"seaweedfs-ui/web"
)

type inMemoryTransport struct {
	handler http.Handler
}

func (t *inMemoryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.handler.ServeHTTP(rec, req)
	return rec.Result(), nil
}

func TestMiddleware_Recovery(t *testing.T) {
	cfg := config.DefaultConfig()
	srv := NewServer(cfg, nil, nil, nil, nil, web.Content)

	panickingHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("simulated critical runtime panic")
	})

	wrapped := srv.recoveryMiddleware(panickingHandler)

	req := httptest.NewRequest(http.MethodGet, "/test-panic", nil)
	rec := httptest.NewRecorder()

	// Should not crash the test; panic should be recovered and return 500 JSON
	wrapped.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d", resp.StatusCode)
	}

	var errResp APIError
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode JSON error: %v", err)
	}

	if errResp.Code != "PANIC_RECOVERED" {
		t.Errorf("expected code PANIC_RECOVERED, got %q", errResp.Code)
	}
}

func TestMiddleware_Tracing(t *testing.T) {
	cfg := config.DefaultConfig()
	srv := NewServer(cfg, nil, nil, nil, nil, web.Content)
	client := &http.Client{Transport: &inMemoryTransport{handler: srv.Handler()}}

	t.Run("AssignsXRequestIDWhenMissing", func(t *testing.T) {
		resp, err := client.Get("http://localhost/api/v1/healthz")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		reqID := resp.Header.Get("X-Request-ID")
		if reqID == "" {
			t.Fatalf("expected X-Request-ID header to be assigned")
		}
	})

	t.Run("PreservesXRequestIDWhenSupplied", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "http://localhost/api/v1/healthz", nil)
		customID := "client-trace-id-998877"
		req.Header.Set("X-Request-ID", customID)

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if id := resp.Header.Get("X-Request-ID"); id != customID {
			t.Fatalf("expected X-Request-ID %q, got %q", customID, id)
		}
	})
}

func TestMiddleware_CORS(t *testing.T) {
	cfg := config.DefaultConfig()
	srv := NewServer(cfg, nil, nil, nil, nil, web.Content)
	client := &http.Client{Transport: &inMemoryTransport{handler: srv.Handler()}}

	t.Run("PreflightOPTIONS", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodOptions, "http://localhost/api/v1/clusters", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("expected 204 No Content for OPTIONS, got %d", resp.StatusCode)
		}

		if origin := resp.Header.Get("Access-Control-Allow-Origin"); origin != "*" {
			t.Errorf("expected Access-Control-Allow-Origin '*', got %q", origin)
		}
		if methods := resp.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(methods, "POST") {
			t.Errorf("expected methods to contain POST, got %q", methods)
		}
	})

	t.Run("GETResponseCORSHeaders", func(t *testing.T) {
		resp, err := client.Get("http://localhost/api/v1/healthz")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if origin := resp.Header.Get("Access-Control-Allow-Origin"); origin != "*" {
			t.Errorf("expected Access-Control-Allow-Origin '*', got %q", origin)
		}
	})
}

func TestMiddleware_RequireAdmin(t *testing.T) {
	t.Run("AdminDisabled_Returns403", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.AllowAdminActions = false
		srv := NewServer(cfg, nil, nil, nil, nil, web.Content)

		called := false
		handler := srv.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
			called = true
		})

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/test-action", nil)
		handler.ServeHTTP(rec, req)

		if called {
			t.Errorf("protected handler was called when admin actions disabled")
		}
		if rec.Code != http.StatusForbidden {
			t.Errorf("expected 403 Forbidden, got %d", rec.Code)
		}
	})

	t.Run("AdminEnabled_Proceeds", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.AllowAdminActions = true
		srv := NewServer(cfg, nil, nil, nil, nil, web.Content)

		called := false
		handler := srv.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
		})

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/test-action", nil)
		handler.ServeHTTP(rec, req)

		if !called {
			t.Errorf("protected handler was NOT called when admin actions enabled")
		}
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200 OK, got %d", rec.Code)
		}
	})
}
