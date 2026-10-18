package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"seaweedfs-ui/internal/api"
)

// TestChallenger_SPA_RootIndexDelivery empirically tests GET / and GET /index.html.
func TestChallenger_SPA_RootIndexDelivery(t *testing.T) {
	h := setupTestServer(t, nil)

	paths := []string{"/", "/index.html"}
	for _, p := range paths {
		t.Run("Path_"+p, func(t *testing.T) {
			resp, err := h.client.Get(h.server.URL + p)
			if err != nil {
				t.Fatalf("GET %s failed: %v", p, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected HTTP 200 OK for %s, got %d", p, resp.StatusCode)
			}

			ct := resp.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "text/html") {
				t.Errorf("expected Content-Type text/html, got %q", ct)
			}

			cc := resp.Header.Get("Cache-Control")
			if !strings.Contains(cc, "no-cache") {
				t.Errorf("expected Cache-Control containing no-cache, got %q", cc)
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("failed to read body: %v", err)
			}
			bodyStr := string(body)

			requiredSubstrings := []string{
				"<!DOCTYPE html>",
				"<html",
				"<title>SeaweedFS Admin Console</title>",
				`<link rel="stylesheet" href="/css/styles.css">`,
				`<script type="module" src="/js/app.js"></script>`,
				`<main id="view-container"`,
				`id="cluster-select"`,
			}
			for _, sub := range requiredSubstrings {
				if !strings.Contains(bodyStr, sub) {
					t.Errorf("expected index.html to contain %q", sub)
				}
			}
		})
	}
}

// TestChallenger_SPA_StylesheetDeliveryAndValidity empirically tests GET /css/styles.css.
func TestChallenger_SPA_StylesheetDeliveryAndValidity(t *testing.T) {
	h := setupTestServer(t, nil)

	resp, err := h.client.Get(h.server.URL + "/css/styles.css")
	if err != nil {
		t.Fatalf("GET /css/styles.css failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 OK for /css/styles.css, got %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/css") {
		t.Errorf("expected Content-Type text/css, got %q", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read body: %v", err)
	}

	if len(body) < 1000 {
		t.Errorf("expected stylesheet to be at least 1000 bytes, got %d", len(body))
	}

	cssStr := string(body)
	requiredCSS := []string{
		":root",
		"--bg-base:",
		"[data-theme=\"light\"]",
		".tabs-nav",
		".badge",
		".row-high-garbage",
		".toast",
		".modal-dialog",
		".drawer",
	}
	for _, req := range requiredCSS {
		if !strings.Contains(cssStr, req) {
			t.Errorf("expected styles.css to contain %q", req)
		}
	}

	// Verify brace balancing in CSS
	openCount := strings.Count(cssStr, "{")
	closeCount := strings.Count(cssStr, "}")
	if openCount == 0 || openCount != closeCount {
		t.Errorf("unbalanced CSS braces: { count=%d, } count=%d", openCount, closeCount)
	}
}

// TestChallenger_SPA_CoreScripts verifies /js/app.js and /js/api.js delivery, MIME, and valid ES module syntax.
func TestChallenger_SPA_CoreScripts(t *testing.T) {
	h := setupTestServer(t, nil)

	scripts := []struct {
		path            string
		expectedExports []string
	}{
		{
			path:            "/js/app.js",
			expectedExports: []string{"appState"},
		},
		{
			path:            "/js/api.js",
			expectedExports: []string{"apiFetch", "showToast", "showConfirmModal", "showDrawer", "formatBytes", "formatLag", "formatDate", "escapeHtml"},
		},
	}

	for _, s := range scripts {
		t.Run("Script_"+s.path, func(t *testing.T) {
			resp, err := h.client.Get(h.server.URL + s.path)
			if err != nil {
				t.Fatalf("GET %s failed: %v", s.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected HTTP 200 OK for %s, got %d", s.path, resp.StatusCode)
			}

			ct := resp.Header.Get("Content-Type")
			expectedCT := "application/javascript; charset=utf-8"
			if ct != expectedCT {
				t.Errorf("expected exact Content-Type %q for %s, got %q", expectedCT, s.path, ct)
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("failed to read body: %v", err)
			}
			if len(body) < 500 {
				t.Errorf("expected script %s to be >=500 bytes, got %d", s.path, len(body))
			}

			bodyStr := string(body)
			for _, exp := range s.expectedExports {
				if !strings.Contains(bodyStr, exp) {
					t.Errorf("script %s missing expected export/identifier %q", s.path, exp)
				}
			}

			// Empirical syntax verification with node -c
			cmd := exec.Command("node", "--input-type=module", "-c")
			cmd.Stdin = strings.NewReader(bodyStr)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("node syntax validation failed for %s: %v, output:\n%s", s.path, err, string(out))
			}
		})
	}
}

// TestChallenger_SPA_All6ViewScripts verifies all 6 view scripts deliver HTTP 200 OK,
// exact Content-Type application/javascript; charset=utf-8, and valid ES module code.
func TestChallenger_SPA_All6ViewScripts(t *testing.T) {
	h := setupTestServer(t, nil)

	views := []struct {
		path         string
		expectedFunc string
	}{
		{"/js/views/overview.js", "renderOverview"},
		{"/js/views/topology.js", "renderTopology"},
		{"/js/views/filersync.js", "renderFilerSync"},
		{"/js/views/s3.js", "renderS3"},
		{"/js/views/filer.js", "renderFiler"},
		{"/js/views/shell.js", "renderShell"},
	}

	for _, v := range views {
		t.Run("View_"+v.path, func(t *testing.T) {
			resp, err := h.client.Get(h.server.URL + v.path)
			if err != nil {
				t.Fatalf("GET %s failed: %v", v.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected HTTP 200 OK for %s, got %d", v.path, resp.StatusCode)
			}

			ct := resp.Header.Get("Content-Type")
			expectedCT := "application/javascript; charset=utf-8"
			if ct != expectedCT {
				t.Errorf("expected exact Content-Type %q for %s, got %q", expectedCT, v.path, ct)
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("failed to read body: %v", err)
			}
			if len(body) < 500 {
				t.Errorf("expected view %s to be at least 500 bytes, got %d", v.path, len(body))
			}

			bodyStr := string(body)
			if !strings.Contains(bodyStr, v.expectedFunc) {
				t.Errorf("view %s missing expected function export %q", v.path, v.expectedFunc)
			}

			// Validate JS syntax using Node.js
			cmd := exec.Command("node", "--input-type=module", "-c")
			cmd.Stdin = strings.NewReader(bodyStr)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("node syntax validation failed for %s: %v, output:\n%s", v.path, err, string(out))
			}
		})
	}
}

// TestChallenger_SPA_ClientRoutesFallback verifies all client routes fallback to index.html with 200 OK.
func TestChallenger_SPA_ClientRoutesFallback(t *testing.T) {
	h := setupTestServer(t, nil)

	clientRoutes := []string{
		"/overview",
		"/topology",
		"/filersync",
		"/s3",
		"/filer",
		"/shell",
		"/nonexistent-client-path",
		"/clusters/test-cluster/topology",
		"/deep/nested/client/route/somewhere",
	}

	for _, route := range clientRoutes {
		t.Run("Route_"+route, func(t *testing.T) {
			resp, err := h.client.Get(h.server.URL + route)
			if err != nil {
				t.Fatalf("GET %s failed: %v", route, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected HTTP 200 OK for client route %s, got %d", route, resp.StatusCode)
			}

			ct := resp.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "text/html") {
				t.Errorf("expected Content-Type text/html for client route %s, got %q", route, ct)
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("failed to read body: %v", err)
			}

			bodyStr := string(body)
			if !strings.Contains(bodyStr, "<title>SeaweedFS Admin Console</title>") {
				t.Errorf("client route %s did not return index.html content", route)
			}
			if !strings.Contains(bodyStr, `<script type="module" src="/js/app.js"></script>`) {
				t.Errorf("client route %s missing script tag", route)
			}
		})
	}
}

// TestChallenger_SPA_APIRoutesNeverFallback verifies that non-existent API routes return JSON 404 and NEVER index.html.
func TestChallenger_SPA_APIRoutesNeverFallback(t *testing.T) {
	h := setupTestServer(t, nil)

	apiRoutes := []string{
		"/api",
		"/api/",
		"/api/v1",
		"/api/v1/",
		"/api/v1/nonexistent",
		"/api/v1/clusters/test-cluster/nonexistent-endpoint",
		"/api/v1/clusters/nonexistent/actions/unknown",
		"/api/v2/foo",
	}

	for _, route := range apiRoutes {
		t.Run("APIRoute_"+route, func(t *testing.T) {
			resp, err := h.client.Get(h.server.URL + route)
			if err != nil {
				t.Fatalf("GET %s failed: %v", route, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("expected HTTP 404 Not Found for %s, got %d", route, resp.StatusCode)
			}

			ct := resp.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") {
				t.Errorf("expected Content-Type application/json for %s, got %q", route, ct)
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("failed to read body: %v", err)
			}
			bodyStr := string(body)

			// NEVER return HTML
			if strings.Contains(bodyStr, "<!DOCTYPE") || strings.Contains(bodyStr, "<html") {
				t.Fatalf("CRITICAL BUG: API route %s returned index.html!", route)
			}

			var errObj api.APIError
			if err := json.Unmarshal(body, &errObj); err != nil {
				t.Fatalf("API 404 response for %s is not valid JSON: %v, body=%q", route, err, bodyStr)
			}

			if errObj.Code != "NOT_FOUND" {
				t.Errorf("expected error code NOT_FOUND for %s, got %q", route, errObj.Code)
			}
		})
	}
}

// TestChallenger_SPA_MissingStaticAssetsNeverFallback verifies missing assets with extensions return 404 JSON, not HTML.
func TestChallenger_SPA_MissingStaticAssetsNeverFallback(t *testing.T) {
	h := setupTestServer(t, nil)

	missingAssets := []string{
		"/css/nonexistent.css",
		"/js/nonexistent.js",
		"/js/views/nonexistent.js",
		"/images/missing.png",
		"/favicon.ico",
	}

	for _, asset := range missingAssets {
		t.Run("Asset_"+asset, func(t *testing.T) {
			resp, err := h.client.Get(h.server.URL + asset)
			if err != nil {
				t.Fatalf("GET %s failed: %v", asset, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("expected HTTP 404 Not Found for missing asset %s, got %d", asset, resp.StatusCode)
			}

			ct := resp.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") {
				t.Errorf("expected Content-Type application/json for missing asset %s, got %q", asset, ct)
			}

			body, _ := io.ReadAll(resp.Body)
			bodyStr := string(body)
			if strings.Contains(bodyStr, "<!DOCTYPE") || strings.Contains(bodyStr, "<html") {
				t.Fatalf("CRITICAL BUG: missing asset %s returned index.html!", asset)
			}

			var errObj api.APIError
			if err := json.Unmarshal(body, &errObj); err != nil {
				t.Fatalf("missing asset 404 response for %s is not valid JSON: %v", asset, err)
			}
			if errObj.Code != "NOT_FOUND" {
				t.Errorf("expected code NOT_FOUND for %s, got %q", asset, errObj.Code)
			}
		})
	}
}

// TestChallenger_SPA_HTTPMethodsAndGuards verifies method guards on static assets and SPA routes.
func TestChallenger_SPA_HTTPMethodsAndGuards(t *testing.T) {
	h := setupTestServer(t, nil)

	// Non-GET/HEAD methods should be rejected with 405 Method Not Allowed
	disallowedMethods := []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch}
	testPaths := []string{"/", "/css/styles.css", "/js/app.js", "/overview"}

	for _, m := range disallowedMethods {
		for _, p := range testPaths {
			t.Run(m+"_"+p, func(t *testing.T) {
				req, err := http.NewRequest(m, h.server.URL+p, nil)
				if err != nil {
					t.Fatalf("failed to create request: %v", err)
				}
				resp, err := h.client.Do(req)
				if err != nil {
					t.Fatalf("request failed: %v", err)
				}
				defer resp.Body.Close()

				if resp.StatusCode != http.StatusMethodNotAllowed {
					t.Errorf("expected 405 Method Not Allowed for %s %s, got %d", m, p, resp.StatusCode)
				}
			})
		}
	}

	// HEAD requests should return 200 with headers and zero body bytes
	for _, p := range []string{"/", "/css/styles.css", "/js/app.js", "/js/views/overview.js"} {
		t.Run("HEAD_"+p, func(t *testing.T) {
			resp, err := h.client.Head(h.server.URL + p)
			if err != nil {
				t.Fatalf("HEAD %s failed: %v", p, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("expected 200 OK for HEAD %s, got %d", p, resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			if len(body) != 0 {
				t.Errorf("expected 0 bytes for HEAD %s, got %d", p, len(body))
			}
		})
	}
}

// TestChallenger_SPA_RealSocketRoundTrip verifies asset serving over a real TCP socket if permitted.
func TestChallenger_SPA_RealSocketRoundTrip(t *testing.T) {
	h := setupTestServer(t, nil)

	realClient := h.server.Client()

	resp, err := realClient.Get(h.server.URL + "/")
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") || strings.Contains(err.Error(), "permission denied") {
			t.Skipf("skipping real TCP socket probe due to sandbox network restrictions: %v", err)
			return
		}
		t.Fatalf("real socket GET failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK for / over TCP, got %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed reading body from TCP socket: %v", err)
	}
	if len(b) == 0 {
		t.Errorf("empty body received over TCP socket")
	}
}

// TestChallenger_SPA_ConcurrentAssetBurst tests concurrent requests under the Go race detector.
func TestChallenger_SPA_ConcurrentAssetBurst(t *testing.T) {
	h := setupTestServer(t, nil)

	paths := []string{
		"/",
		"/index.html",
		"/css/styles.css",
		"/js/app.js",
		"/js/api.js",
		"/js/views/overview.js",
		"/js/views/topology.js",
		"/js/views/filersync.js",
		"/js/views/s3.js",
		"/js/views/filer.js",
		"/js/views/shell.js",
		"/overview",
		"/topology",
		"/api/v1/healthz",
		"/api/v1/nonexistent",
		"/css/missing.css",
	}

	const concurrency = 50
	var wg sync.WaitGroup
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		targetPath := paths[i%len(paths)]
		go func(p string) {
			defer wg.Done()
			resp, err := h.client.Get(h.server.URL + p)
			if err != nil {
				t.Errorf("concurrent GET %s failed: %v", p, err)
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
		}(targetPath)
	}

	wg.Wait()
}
