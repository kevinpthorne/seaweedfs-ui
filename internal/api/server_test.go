package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"seaweedfs-ui/internal/api"
	"seaweedfs-ui/internal/cluster"
	"seaweedfs-ui/internal/config"
	"seaweedfs-ui/internal/filersync"
	"seaweedfs-ui/internal/weedcli"
	"seaweedfs-ui/internal/weedhttp"
	"seaweedfs-ui/web"
)

// --- Mocks ---

type mockRegistry struct {
	mu       sync.RWMutex
	clusters []cluster.ClusterTarget
}

func (m *mockRegistry) ListClusters(ctx context.Context) ([]cluster.ClusterTarget, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.clusters, nil
}

func (m *mockRegistry) GetCluster(ctx context.Context, id string) (*cluster.ClusterTarget, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, c := range m.clusters {
		if c.ID == id {
			clone := c
			return &clone, nil
		}
	}
	return nil, fmt.Errorf("cluster not found: %s", id)
}

func (m *mockRegistry) Refresh(ctx context.Context) error { return nil }
func (m *mockRegistry) Start(ctx context.Context) error   { return nil }
func (m *mockRegistry) Stop()                             {}

type mockWeedCLI struct {
	mu          sync.Mutex
	methodCalls []string
	lastCommand string
}

func (m *mockWeedCLI) record(method string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.methodCalls = append(m.methodCalls, method)
}

func (m *mockWeedCLI) CallCount(method string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, call := range m.methodCalls {
		if call == method {
			count++
		}
	}
	return count
}

func (m *mockWeedCLI) Vacuum(ctx context.Context, target cluster.ClusterTarget, col string) (*weedcli.ShellResult, error) {
	m.record("Vacuum")
	return &weedcli.ShellResult{Stdout: "vacuum completed", DurationMs: 15}, nil
}

func (m *mockWeedCLI) Balance(ctx context.Context, target cluster.ClusterTarget) (*weedcli.ShellResult, error) {
	m.record("Balance")
	return &weedcli.ShellResult{Stdout: "balance completed", DurationMs: 25}, nil
}

func (m *mockWeedCLI) FixReplication(ctx context.Context, target cluster.ClusterTarget) (*weedcli.ShellResult, error) {
	m.record("FixReplication")
	return &weedcli.ShellResult{Stdout: "fix replication completed", DurationMs: 30}, nil
}

func (m *mockWeedCLI) Grow(ctx context.Context, target cluster.ClusterTarget, count int, col, rep, dc string) (*weedcli.ShellResult, error) {
	m.record("Grow")
	return &weedcli.ShellResult{Stdout: fmt.Sprintf("grew %d volumes", count), DurationMs: 40}, nil
}

func (m *mockWeedCLI) ListBuckets(ctx context.Context, target cluster.ClusterTarget) ([]string, error) {
	m.record("ListBuckets")
	return []string{"photos", "backups"}, nil
}

func (m *mockWeedCLI) CreateBucket(ctx context.Context, target cluster.ClusterTarget, name string) (*weedcli.ShellResult, error) {
	m.record("CreateBucket")
	return &weedcli.ShellResult{Stdout: "bucket created", DurationMs: 10}, nil
}

func (m *mockWeedCLI) DeleteBucket(ctx context.Context, target cluster.ClusterTarget, name string) (*weedcli.ShellResult, error) {
	m.record("DeleteBucket")
	return &weedcli.ShellResult{Stdout: "bucket deleted", DurationMs: 12}, nil
}

func (m *mockWeedCLI) ListUsers(ctx context.Context, target cluster.ClusterTarget) ([]weedcli.S3User, error) {
	m.record("ListUsers")
	return []weedcli.S3User{
		{Name: "admin", Actions: []string{"Admin", "Read", "Write"}},
	}, nil
}

func (m *mockWeedCLI) MetaCat(ctx context.Context, target cluster.ClusterTarget, path string) (string, error) {
	m.record("MetaCat")
	return "inode: 12345, chunks: 3", nil
}

func (m *mockWeedCLI) VolumeList(ctx context.Context, target cluster.ClusterTarget, collection string) (*weedcli.VolumeTopology, error) {
	m.record("VolumeList")
	return &weedcli.VolumeTopology{}, nil
}

func (m *mockWeedCLI) Execute(ctx context.Context, target cluster.ClusterTarget, command string, isRaw bool) (*weedcli.ShellResult, error) {
	m.record("Execute")
	m.mu.Lock()
	m.lastCommand = command
	m.mu.Unlock()
	return &weedcli.ShellResult{Stdout: "command executed: " + command, DurationMs: 8}, nil
}

type mockWeedHTTP struct{}

func (m *mockWeedHTTP) GetClusterStatus(ctx context.Context, target cluster.ClusterTarget) (*weedhttp.ClusterStatus, error) {
	return &weedhttp.ClusterStatus{
		IsLeader: true,
		Leader:   "10.0.0.1:9333",
		Peers:    []string{"10.0.0.1:9333", "10.0.0.2:9333"},
	}, nil
}

func (m *mockWeedHTTP) GetTopology(ctx context.Context, target cluster.ClusterTarget) (*weedhttp.Topology, error) {
	return &weedhttp.Topology{
		MaxVolumeSlots:  100,
		FreeVolumeSlots: 40,
		DataCenters: []weedhttp.DirDataCenter{
			{Id: "DefaultDataCenter"},
		},
	}, nil
}

func (m *mockWeedHTTP) GetVolumes(ctx context.Context, target cluster.ClusterTarget) ([]weedhttp.VolumeInfo, error) {
	return []weedhttp.VolumeInfo{
		{ID: 1, Size: 1048576, Collection: "swfs", ReplicaPlacement: "001", GarbageRatio: 0.35, ReadOnly: false},
		{ID: 2, Size: 2048576, Collection: "logs", ReplicaPlacement: "000", GarbageRatio: 0.05, ReadOnly: true},
	}, nil
}

func (m *mockWeedHTTP) ListFilerDir(ctx context.Context, target cluster.ClusterTarget, path string, limit int) (*weedhttp.FilerDirResult, error) {
	return &weedhttp.FilerDirResult{
		Path: path,
		Entries: []weedhttp.FilerEntry{
			{FullPath: "/test.txt", Mtime: time.Now().UTC(), Mode: 0644},
		},
	}, nil
}

func (m *mockWeedHTTP) StreamFile(ctx context.Context, target cluster.ClusterTarget, path string) (io.ReadCloser, string, error) {
	return io.NopCloser(strings.NewReader("file content")), "text/plain", nil
}

func (m *mockWeedHTTP) DeleteEntry(ctx context.Context, target cluster.ClusterTarget, path string, isDir bool) error {
	return nil
}

func (m *mockWeedHTTP) GetDirStatus(ctx context.Context, target cluster.ClusterTarget) (*weedhttp.DirStatusResponse, error) {
	return &weedhttp.DirStatusResponse{}, nil
}

func (m *mockWeedHTTP) GetVolumeStatus(ctx context.Context, target cluster.ClusterTarget) (*weedhttp.VolumeStatusResponse, error) {
	return &weedhttp.VolumeStatusResponse{}, nil
}

func (m *mockWeedHTTP) UploadFile(ctx context.Context, target cluster.ClusterTarget, dir, name string, r io.Reader, mime string) error {
	return nil
}

func (m *mockWeedHTTP) CheckS3Status(ctx context.Context, target cluster.ClusterTarget) (*weedhttp.S3EndpointStatus, error) {
	return &weedhttp.S3EndpointStatus{Available: true, Endpoint: "10.0.0.1:8333"}, nil
}

func (m *mockWeedHTTP) GetVolumeServerStatus(ctx context.Context, target cluster.ClusterTarget, addr string) (*weedhttp.VolumeServerStatus, error) {
	return &weedhttp.VolumeServerStatus{}, nil
}

type mockFilerSync struct{}

func (m *mockFilerSync) InspectSyncJobs(ctx context.Context, target cluster.ClusterTarget) ([]filersync.SyncStatus, error) {
	return []filersync.SyncStatus{
		{
			SourceFiler:           "filer-a:8888",
			TargetFiler:           "filer-b:8888",
			SourcePath:            "/",
			TargetPath:            "/",
			Mode:                  "Active-Active",
			ReplicationLagSeconds: 0.8,
			SyncedFileCount:       1250,
			BytesPerSecond:        15.5,
			Status:                "HEALTHY",
		},
	}, nil
}

type inMemoryTransport struct {
	handler http.Handler
}

func (t *inMemoryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.handler.ServeHTTP(rec, req)
	return rec.Result(), nil
}

type testHarness struct {
	server   *httptest.Server
	client   *http.Client
	handler  http.Handler
	cfg      *config.Config
	registry *mockRegistry
	weedcli  *mockWeedCLI
	weedhttp *mockWeedHTTP
	fsync    *mockFilerSync
}

func setupTestServer(t *testing.T, modifyCfg func(c *config.Config)) *testHarness {
	cfg := config.DefaultConfig()
	if modifyCfg != nil {
		modifyCfg(cfg)
	}

	reg := &mockRegistry{
		clusters: []cluster.ClusterTarget{
			{
				ID:      "test-cluster",
				Name:    "Test Cluster",
				Source:  cluster.SourceStatic,
				Masters: []string{"127.0.0.1:9333"},
				Filer:   "127.0.0.1:8888",
				S3:      "127.0.0.1:8333",
			},
		},
	}
	cli := &mockWeedCLI{}
	whttp := &mockWeedHTTP{}
	fsync := &mockFilerSync{}

	srv := api.NewServer(cfg, reg, cli, whttp, fsync, web.Content, "0.1.0")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &testHarness{
		server:   ts,
		client:   &http.Client{Transport: &inMemoryTransport{handler: srv.Handler()}},
		handler:  srv.Handler(),
		cfg:      cfg,
		registry: reg,
		weedcli:  cli,
		weedhttp: whttp,
		fsync:    fsync,
	}
}

// --- Test Suites ---

func TestAPI_All15Routes_HappyPath(t *testing.T) {
	h := setupTestServer(t, func(c *config.Config) {
		c.AllowAdminActions = true
		c.EnableRawShell = true
	})

	testCases := []struct {
		name         string
		method       string
		path         string
		body         string
		expectedCode int
		checkBody    string
	}{
		{"Healthz", http.MethodGet, "/api/v1/healthz", "", http.StatusOK, `"status":"ok"`},
		{"Config", http.MethodGet, "/api/v1/config", "", http.StatusOK, `"allowAdminActions":true`},
		{"Clusters", http.MethodGet, "/api/v1/clusters", "", http.StatusOK, `"id":"test-cluster"`},
		{"Overview", http.MethodGet, "/api/v1/clusters/test-cluster/overview", "", http.StatusOK, `"leader":"10.0.0.1:9333"`},
		{"Topology", http.MethodGet, "/api/v1/clusters/test-cluster/topology", "", http.StatusOK, `"dataCenters"`},
		{"Volumes", http.MethodGet, "/api/v1/clusters/test-cluster/volumes", "", http.StatusOK, `"collection":"swfs"`},
		{"VolumesFiltered", http.MethodGet, "/api/v1/clusters/test-cluster/volumes?collection=swfs&readOnly=false&highGarbage=true", "", http.StatusOK, `"collection":"swfs"`},
		{"ActionVacuum", http.MethodPost, "/api/v1/clusters/test-cluster/actions/vacuum", `{"collection":"swfs"}`, http.StatusOK, `"status":"completed"`},
		{"ActionBalance", http.MethodPost, "/api/v1/clusters/test-cluster/actions/balance", `{}`, http.StatusOK, `"status":"completed"`},
		{"ActionFixReplication", http.MethodPost, "/api/v1/clusters/test-cluster/actions/fix.replication", `{}`, http.StatusOK, `"status":"completed"`},
		{"ActionGrow", http.MethodPost, "/api/v1/clusters/test-cluster/actions/volume.grow", `{"count":2}`, http.StatusOK, `"status":"completed"`},
		{"FilerSync", http.MethodGet, "/api/v1/clusters/test-cluster/filersync", "", http.StatusOK, `"replicationLagSeconds":0.8`},
		{"S3ListBuckets", http.MethodGet, "/api/v1/clusters/test-cluster/s3/buckets", "", http.StatusOK, `"photos"`},
		{"S3CreateBucket", http.MethodPost, "/api/v1/clusters/test-cluster/s3/buckets", `{"name":"new-bucket"}`, http.StatusCreated, `"created"`},
		{"S3DeleteBucket", http.MethodDelete, "/api/v1/clusters/test-cluster/s3/buckets/old-bucket", "", http.StatusOK, `"deleted"`},
		{"S3ListUsers", http.MethodGet, "/api/v1/clusters/test-cluster/s3/users", "", http.StatusOK, `"name":"admin"`},
		{"FilerDir", http.MethodGet, "/api/v1/clusters/test-cluster/filer?path=/", "", http.StatusOK, `test.txt`},
		{"FilerMeta", http.MethodGet, "/api/v1/clusters/test-cluster/filer/meta?path=/test.txt", "", http.StatusOK, `12345`},
		{"ShellExecPreset", http.MethodPost, "/api/v1/clusters/test-cluster/shell", `{"command":"volume.list"}`, http.StatusOK, `volume.list`},
		{"ShellExecRaw", http.MethodPost, "/api/v1/clusters/test-cluster/shell", `{"command":"arbitrary.status"}`, http.StatusOK, `arbitrary.status`},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var bodyReader io.Reader
			if tc.body != "" {
				bodyReader = bytes.NewBufferString(tc.body)
			}
			req, err := http.NewRequest(tc.method, h.server.URL+tc.path, bodyReader)
			if err != nil {
				t.Fatalf("failed to create request: %v", err)
			}
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}

			resp, err := h.client.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.expectedCode {
				bodyBytes, _ := io.ReadAll(resp.Body)
				t.Fatalf("expected status %d, got %d. Body: %s", tc.expectedCode, resp.StatusCode, string(bodyBytes))
			}

			if reqID := resp.Header.Get("X-Request-ID"); reqID == "" {
				t.Errorf("missing X-Request-ID header in response")
			}

			bodyBytes, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(bodyBytes), tc.checkBody) {
				t.Errorf("response body does not contain %q: %s", tc.checkBody, string(bodyBytes))
			}
		})
	}
}

func TestAPI_AdminGuardrails_MutatingForbidden(t *testing.T) {
	h := setupTestServer(t, func(c *config.Config) {
		c.AllowAdminActions = false
	})

	mutatingRequests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"Vacuum", http.MethodPost, "/api/v1/clusters/test-cluster/actions/vacuum", `{"collection":"swfs"}`},
		{"Balance", http.MethodPost, "/api/v1/clusters/test-cluster/actions/balance", `{}`},
		{"FixReplication", http.MethodPost, "/api/v1/clusters/test-cluster/actions/fix.replication", `{}`},
		{"Grow", http.MethodPost, "/api/v1/clusters/test-cluster/actions/volume.grow", `{"count":1}`},
		{"CreateBucket", http.MethodPost, "/api/v1/clusters/test-cluster/s3/buckets", `{"name":"forbidden-bucket"}`},
		{"DeleteBucket", http.MethodDelete, "/api/v1/clusters/test-cluster/s3/buckets/forbidden-bucket", ""},
	}

	for _, tc := range mutatingRequests {
		t.Run(tc.name, func(t *testing.T) {
			var bodyReader io.Reader
			if tc.body != "" {
				bodyReader = bytes.NewBufferString(tc.body)
			}
			req, err := http.NewRequest(tc.method, h.server.URL+tc.path, bodyReader)
			if err != nil {
				t.Fatalf("failed to create request: %v", err)
			}
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}

			resp, err := h.client.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusForbidden {
				bodyBytes, _ := io.ReadAll(resp.Body)
				t.Errorf("expected 403 Forbidden, got %d. Body: %s", resp.StatusCode, string(bodyBytes))
			}

			var errResp api.APIError
			if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
				t.Fatalf("failed to parse structured error JSON: %v", err)
			}
			if errResp.Code != "ADMIN_ACTIONS_DISABLED" {
				t.Errorf("expected error code ADMIN_ACTIONS_DISABLED, got %q", errResp.Code)
			}
		})
	}

	// CRITICAL ASSERTION: Zero mutating CLI calls
	if count := h.weedcli.CallCount("Vacuum"); count != 0 {
		t.Errorf("Vacuum was called %d times; expected 0", count)
	}
	if count := h.weedcli.CallCount("Balance"); count != 0 {
		t.Errorf("Balance was called %d times; expected 0", count)
	}
	if count := h.weedcli.CallCount("FixReplication"); count != 0 {
		t.Errorf("FixReplication was called %d times; expected 0", count)
	}
	if count := h.weedcli.CallCount("Grow"); count != 0 {
		t.Errorf("Grow was called %d times; expected 0", count)
	}
	if count := h.weedcli.CallCount("CreateBucket"); count != 0 {
		t.Errorf("CreateBucket was called %d times; expected 0", count)
	}
	if count := h.weedcli.CallCount("DeleteBucket"); count != 0 {
		t.Errorf("DeleteBucket was called %d times; expected 0", count)
	}
}

func TestAPI_RawShellGuard_ForbiddenAndPresets(t *testing.T) {
	h := setupTestServer(t, func(c *config.Config) {
		c.EnableRawShell = false
		c.AllowAdminActions = false
	})

	t.Run("ArbitraryRawCommand_Forbidden", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/api/v1/clusters/test-cluster/shell", strings.NewReader(`{"command":"arbitrary.cmd"}`))
		req.Header.Set("Content-Type", "application/json")

		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("expected 403 Forbidden, got %d", resp.StatusCode)
		}
		var errResp api.APIError
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		if errResp.Code != "RAW_SHELL_DISABLED" {
			t.Errorf("expected code RAW_SHELL_DISABLED, got %q", errResp.Code)
		}
	})

	t.Run("SafePresetCommand_Allowed", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/api/v1/clusters/test-cluster/shell", strings.NewReader(`{"command":"volume.list"}`))
		req.Header.Set("Content-Type", "application/json")

		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200 OK for safe preset, got %d", resp.StatusCode)
		}
	})

	t.Run("RawShellEnabled_MutatingWithAdminDisabled_Forbidden", func(t *testing.T) {
		hRaw := setupTestServer(t, func(c *config.Config) {
			c.EnableRawShell = true
			c.AllowAdminActions = false
		})

		req, _ := http.NewRequest(http.MethodPost, hRaw.server.URL+"/api/v1/clusters/test-cluster/shell", strings.NewReader(`{"command":"volume.delete -volumeId=1"}`))
		req.Header.Set("Content-Type", "application/json")

		resp, err := hRaw.client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("expected 403 Forbidden for mutating shell with admin disabled, got %d", resp.StatusCode)
		}
		var errResp api.APIError
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		if errResp.Code != "ADMIN_ACTIONS_DISABLED" {
			t.Errorf("expected code ADMIN_ACTIONS_DISABLED, got %q", errResp.Code)
		}
	})
}

func TestAPI_SPA_AssetServing(t *testing.T) {
	h := setupTestServer(t, nil)

	assets := []struct {
		path           string
		expectedStatus int
		expectedMime   string
		exactMatch     bool
	}{
		{"/", http.StatusOK, "text/html", false},
		{"/index.html", http.StatusOK, "text/html", false},
		{"/css/styles.css", http.StatusOK, "text/css", false},
		{"/js/app.js", http.StatusOK, "application/javascript; charset=utf-8", true},
		{"/js/api.js", http.StatusOK, "application/javascript; charset=utf-8", true},
		{"/js/views/overview.js", http.StatusOK, "application/javascript; charset=utf-8", true},
		{"/js/views/topology.js", http.StatusOK, "application/javascript; charset=utf-8", true},
		{"/js/views/filersync.js", http.StatusOK, "application/javascript; charset=utf-8", true},
		{"/js/views/s3.js", http.StatusOK, "application/javascript; charset=utf-8", true},
		{"/js/views/filer.js", http.StatusOK, "application/javascript; charset=utf-8", true},
		{"/js/views/shell.js", http.StatusOK, "application/javascript; charset=utf-8", true},
	}

	for _, a := range assets {
		t.Run(a.path, func(t *testing.T) {
			resp, err := h.client.Get(h.server.URL + a.path)
			if err != nil {
				t.Fatalf("request %s failed: %v", a.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != a.expectedStatus {
				t.Errorf("expected %d for %s, got %d", a.expectedStatus, a.path, resp.StatusCode)
			}

			contentType := resp.Header.Get("Content-Type")
			if a.exactMatch {
				if contentType != a.expectedMime {
					t.Errorf("expected exact Content-Type %q for %s, got %q", a.expectedMime, a.path, contentType)
				}
			} else {
				if !strings.Contains(contentType, a.expectedMime) {
					t.Errorf("expected Content-Type containing %q for %s, got %q", a.expectedMime, a.path, contentType)
				}
			}

			bodyBytes, _ := io.ReadAll(resp.Body)
			if len(bodyBytes) == 0 {
				t.Errorf("empty body returned for %s", a.path)
			}
		})
	}
}

func TestAPI_SPA_FallbackRouting(t *testing.T) {
	h := setupTestServer(t, nil)

	clientRoutes := []string{
		"/overview",
		"/topology",
		"/filersync",
		"/s3",
		"/filer",
		"/shell",
		"/clusters/prod/topology",
	}

	for _, route := range clientRoutes {
		t.Run("Fallback_"+route, func(t *testing.T) {
			resp, err := h.client.Get(h.server.URL + route)
			if err != nil {
				t.Fatalf("request %s failed: %v", route, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("expected 200 OK for SPA fallback on %s, got %d", route, resp.StatusCode)
			}

			contentType := resp.Header.Get("Content-Type")
			if !strings.Contains(contentType, "text/html") {
				t.Errorf("expected text/html for SPA fallback, got %q", contentType)
			}
		})
	}

	t.Run("NonExistentAPIRoute_Returns404JSON", func(t *testing.T) {
		resp, err := h.client.Get(h.server.URL + "/api/v1/doesnotexist")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 for unknown API route, got %d", resp.StatusCode)
		}
		contentType := resp.Header.Get("Content-Type")
		if !strings.Contains(contentType, "application/json") {
			t.Errorf("expected application/json for API 404, got %q", contentType)
		}
	})

	t.Run("MissingStaticFile_Returns404JSON", func(t *testing.T) {
		resp, err := h.client.Get(h.server.URL + "/css/nonexistent.css")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 for missing static asset, got %d", resp.StatusCode)
		}
	})
}

func TestAPI_StructuredErrors(t *testing.T) {
	h := setupTestServer(t, func(c *config.Config) {
		c.AllowAdminActions = true
		c.EnableRawShell = true
	})

	t.Run("ClusterNotFound", func(t *testing.T) {
		resp, err := h.client.Get(h.server.URL + "/api/v1/clusters/non-existent-id/overview")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404, got %d", resp.StatusCode)
		}
		var errResp api.APIError
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		if errResp.Code != "CLUSTER_NOT_FOUND" {
			t.Errorf("expected code CLUSTER_NOT_FOUND, got %q", errResp.Code)
		}
	})

	t.Run("InvalidActionName", func(t *testing.T) {
		resp, err := h.client.Post(h.server.URL+"/api/v1/clusters/test-cluster/actions/invalid_action", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400 for invalid action, got %d", resp.StatusCode)
		}
		var errResp api.APIError
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		if errResp.Code != "INVALID_ARGUMENT" {
			t.Errorf("expected code INVALID_ARGUMENT, got %q", errResp.Code)
		}
	})

	t.Run("EmptyShellCommand", func(t *testing.T) {
		resp, err := h.client.Post(h.server.URL+"/api/v1/clusters/test-cluster/shell", "application/json", strings.NewReader(`{"command":""}`))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400 for empty shell command, got %d", resp.StatusCode)
		}
	})

	t.Run("InvalidBucketName", func(t *testing.T) {
		resp, err := h.client.Post(h.server.URL+"/api/v1/clusters/test-cluster/s3/buckets", "application/json", strings.NewReader(`{"name":"INVALID BUCKET!!"}`))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400 for invalid bucket name, got %d", resp.StatusCode)
		}
	})
}

func TestAPI_ConcurrentRaceSafety(t *testing.T) {
	h := setupTestServer(t, func(c *config.Config) {
		c.AllowAdminActions = true
		c.EnableRawShell = true
	})

	var wg sync.WaitGroup
	iterations := 50

	for i := 0; i < iterations; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			endpoint := "/api/v1/healthz"
			switch n % 5 {
			case 0:
				endpoint = "/api/v1/clusters"
			case 1:
				endpoint = "/api/v1/config"
			case 2:
				endpoint = "/api/v1/clusters/test-cluster/overview"
			case 3:
				endpoint = "/api/v1/clusters/test-cluster/volumes"
			}

			req := httptest.NewRequest(http.MethodGet, endpoint, nil)
			rec := httptest.NewRecorder()
			h.handler.ServeHTTP(rec, req)

			resp := rec.Result()
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("concurrent GET %s failed with status: %d", endpoint, resp.StatusCode)
			}
		}(i)
	}

	wg.Wait()
}
