package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"seaweedfs-ui/internal/cluster"
	"seaweedfs-ui/internal/config"
	"seaweedfs-ui/internal/filersync"
	"seaweedfs-ui/internal/weedcli"
	"seaweedfs-ui/internal/weedhttp"
	"seaweedfs-ui/web"
)

// --- Adversarial Test Mocks ---

type advMockRegistry struct {
	mu       sync.RWMutex
	clusters []cluster.ClusterTarget
}

func (m *advMockRegistry) ListClusters(ctx context.Context) ([]cluster.ClusterTarget, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.clusters, nil
}

func (m *advMockRegistry) GetCluster(ctx context.Context, id string) (*cluster.ClusterTarget, error) {
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

func (m *advMockRegistry) Refresh(ctx context.Context) error { return nil }
func (m *advMockRegistry) Start(ctx context.Context) error   { return nil }
func (m *advMockRegistry) Stop()                             {}

type advMockWeedCLI struct {
	mu          sync.Mutex
	methodCalls []string
	lastCommand string
}

func (m *advMockWeedCLI) record(method string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.methodCalls = append(m.methodCalls, method)
}

func (m *advMockWeedCLI) CallCount(method string) int {
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

func (m *advMockWeedCLI) TotalCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.methodCalls)
}

func (m *advMockWeedCLI) Vacuum(ctx context.Context, target cluster.ClusterTarget, col string) (*weedcli.ShellResult, error) {
	m.record("Vacuum")
	return &weedcli.ShellResult{Stdout: "vacuum done", DurationMs: 10}, nil
}

func (m *advMockWeedCLI) Balance(ctx context.Context, target cluster.ClusterTarget) (*weedcli.ShellResult, error) {
	m.record("Balance")
	return &weedcli.ShellResult{Stdout: "balance done", DurationMs: 10}, nil
}

func (m *advMockWeedCLI) FixReplication(ctx context.Context, target cluster.ClusterTarget) (*weedcli.ShellResult, error) {
	m.record("FixReplication")
	return &weedcli.ShellResult{Stdout: "fix done", DurationMs: 10}, nil
}

func (m *advMockWeedCLI) Grow(ctx context.Context, target cluster.ClusterTarget, count int, col, rep, dc string) (*weedcli.ShellResult, error) {
	m.record("Grow")
	return &weedcli.ShellResult{Stdout: fmt.Sprintf("grow %d done", count), DurationMs: 10}, nil
}

func (m *advMockWeedCLI) ListBuckets(ctx context.Context, target cluster.ClusterTarget) ([]string, error) {
	m.record("ListBuckets")
	return []string{"photos", "backups"}, nil
}

func (m *advMockWeedCLI) CreateBucket(ctx context.Context, target cluster.ClusterTarget, name string) (*weedcli.ShellResult, error) {
	m.record("CreateBucket")
	return &weedcli.ShellResult{Stdout: "bucket created", DurationMs: 10}, nil
}

func (m *advMockWeedCLI) DeleteBucket(ctx context.Context, target cluster.ClusterTarget, name string) (*weedcli.ShellResult, error) {
	m.record("DeleteBucket")
	return &weedcli.ShellResult{Stdout: "bucket deleted", DurationMs: 10}, nil
}

func (m *advMockWeedCLI) ListUsers(ctx context.Context, target cluster.ClusterTarget) ([]weedcli.S3User, error) {
	m.record("ListUsers")
	return []weedcli.S3User{{Name: "admin"}}, nil
}

func (m *advMockWeedCLI) MetaCat(ctx context.Context, target cluster.ClusterTarget, path string) (string, error) {
	m.record("MetaCat")
	return "inode: 123", nil
}

func (m *advMockWeedCLI) VolumeList(ctx context.Context, target cluster.ClusterTarget, collection string) (*weedcli.VolumeTopology, error) {
	m.record("VolumeList")
	return &weedcli.VolumeTopology{}, nil
}

func (m *advMockWeedCLI) Execute(ctx context.Context, target cluster.ClusterTarget, command string, isRaw bool) (*weedcli.ShellResult, error) {
	m.record("Execute")
	m.mu.Lock()
	m.lastCommand = command
	m.mu.Unlock()
	return &weedcli.ShellResult{Stdout: "executed: " + command, DurationMs: 5}, nil
}

type advMockWeedHTTP struct{}

func (m *advMockWeedHTTP) GetClusterStatus(ctx context.Context, target cluster.ClusterTarget) (*weedhttp.ClusterStatus, error) {
	return &weedhttp.ClusterStatus{IsLeader: true, Leader: "127.0.0.1:9333", Peers: []string{"127.0.0.1:9333"}}, nil
}
func (m *advMockWeedHTTP) GetTopology(ctx context.Context, target cluster.ClusterTarget) (*weedhttp.Topology, error) {
	return &weedhttp.Topology{MaxVolumeSlots: 100, FreeVolumeSlots: 50}, nil
}
func (m *advMockWeedHTTP) GetVolumes(ctx context.Context, target cluster.ClusterTarget) ([]weedhttp.VolumeInfo, error) {
	return []weedhttp.VolumeInfo{{ID: 1, Collection: "test", Size: 1024}}, nil
}
func (m *advMockWeedHTTP) ListFilerDir(ctx context.Context, target cluster.ClusterTarget, path string, limit int) (*weedhttp.FilerDirResult, error) {
	return &weedhttp.FilerDirResult{Path: path, Entries: []weedhttp.FilerEntry{}}, nil
}
func (m *advMockWeedHTTP) StreamFile(ctx context.Context, target cluster.ClusterTarget, path string) (io.ReadCloser, string, error) {
	return io.NopCloser(strings.NewReader("content")), "text/plain", nil
}
func (m *advMockWeedHTTP) DeleteEntry(ctx context.Context, target cluster.ClusterTarget, path string, isDir bool) error {
	return nil
}
func (m *advMockWeedHTTP) GetDirStatus(ctx context.Context, target cluster.ClusterTarget) (*weedhttp.DirStatusResponse, error) {
	return &weedhttp.DirStatusResponse{}, nil
}
func (m *advMockWeedHTTP) GetVolumeStatus(ctx context.Context, target cluster.ClusterTarget) (*weedhttp.VolumeStatusResponse, error) {
	return &weedhttp.VolumeStatusResponse{}, nil
}
func (m *advMockWeedHTTP) UploadFile(ctx context.Context, target cluster.ClusterTarget, dir, name string, r io.Reader, mime string) error {
	return nil
}
func (m *advMockWeedHTTP) CheckS3Status(ctx context.Context, target cluster.ClusterTarget) (*weedhttp.S3EndpointStatus, error) {
	return &weedhttp.S3EndpointStatus{Available: true, Endpoint: "127.0.0.1:8333"}, nil
}
func (m *advMockWeedHTTP) GetVolumeServerStatus(ctx context.Context, target cluster.ClusterTarget, addr string) (*weedhttp.VolumeServerStatus, error) {
	return &weedhttp.VolumeServerStatus{}, nil
}

type advMockFilerSync struct{}

func (m *advMockFilerSync) InspectSyncJobs(ctx context.Context, target cluster.ClusterTarget) ([]filersync.SyncStatus, error) {
	return []filersync.SyncStatus{{SourceFiler: "f1", TargetFiler: "f2", Status: "HEALTHY"}}, nil
}

type advHarness struct {
	server   *httptest.Server
	client   *http.Client
	handler  http.Handler
	srv      *Server
	cfg      *config.Config
	registry *advMockRegistry
	weedcli  *advMockWeedCLI
}

func setupAdvServer(t *testing.T, modifyCfg func(c *config.Config)) *advHarness {
	cfg := config.DefaultConfig()
	if modifyCfg != nil {
		modifyCfg(cfg)
	}

	reg := &advMockRegistry{
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
	cli := &advMockWeedCLI{}
	whttp := &advMockWeedHTTP{}
	fsync := &advMockFilerSync{}

	srv := NewServer(cfg, reg, cli, whttp, fsync, web.Content, "0.1.0")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &advHarness{
		server:   ts,
		client:   &http.Client{Transport: &inMemoryTransport{handler: srv.Handler()}},
		handler:  srv.Handler(),
		srv:      srv,
		cfg:      cfg,
		registry: reg,
		weedcli:  cli,
	}
}

// ============================================================================
// SUITE 1: Mutating Actions Guardrail (AllowAdminActions=false)
// ============================================================================

func TestAdversarial_MutatingActions_ForbiddenWhenAdminDisabled(t *testing.T) {
	h := setupAdvServer(t, func(c *config.Config) {
		c.AllowAdminActions = false
		c.EnableRawShell = false
	})

	mutatingActions := []string{
		"vacuum",
		"balance",
		"fix.replication",
		"volume.grow",
	}

	payloads := []struct {
		desc string
		body string
	}{
		{"EmptyBody", ""},
		{"EmptyJSON", "{}"},
		{"ValidArgs", `{"collection":"bench","replication":"001","dataCenter":"dc1","count":5}`},
		{"HostileSemicolonInjection", `{"collection":"bench; rm -rf /","replication":"000"}`},
		{"HostilePipeInjection", `{"collection":"bench | cat /etc/passwd"}`},
		{"HostileBacktickInjection", "{\"collection\":\"`whoami`\"}"},
		{"HostileSubshellInjection", `{"collection":"$(id)"}`},
		{"HostileNewlineInjection", "{\"collection\":\"bench\\nwhoami\"}"},
		{"MalformedJSON", `{"collection": `},
	}

	for _, action := range mutatingActions {
		for _, pl := range payloads {
			name := fmt.Sprintf("Action_%s/%s", action, pl.desc)
			t.Run(name, func(t *testing.T) {
				url := fmt.Sprintf("%s/api/v1/clusters/test-cluster/actions/%s", h.server.URL, action)
				var bodyReader io.Reader
				if pl.body != "" {
					bodyReader = strings.NewReader(pl.body)
				}
				req, err := http.NewRequest(http.MethodPost, url, bodyReader)
				if err != nil {
					t.Fatalf("failed to build request: %v", err)
				}
				if pl.body != "" {
					req.Header.Set("Content-Type", "application/json")
				}

				resp, err := h.client.Do(req)
				if err != nil {
					t.Fatalf("request failed: %v", err)
				}
				defer resp.Body.Close()

				// 1. Must return 403 Forbidden
				if resp.StatusCode != http.StatusForbidden {
					body, _ := io.ReadAll(resp.Body)
					t.Fatalf("expected 403 Forbidden for action %q with %s, got %d. Body: %s",
						action, pl.desc, resp.StatusCode, string(body))
				}

				// 2. Content-Type must be application/json
				contentType := resp.Header.Get("Content-Type")
				if !strings.Contains(contentType, "application/json") {
					t.Errorf("expected application/json, got %q", contentType)
				}

				// 3. Error code must be ADMIN_ACTIONS_DISABLED
				var errResp APIError
				if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
					t.Fatalf("failed to parse structured error JSON: %v", err)
				}
				if errResp.Code != "ADMIN_ACTIONS_DISABLED" {
					t.Errorf("expected error code ADMIN_ACTIONS_DISABLED, got %q", errResp.Code)
				}
			})
		}
	}

	// CRITICAL SECURITY ASSERTION: Under no circumstances should weedcli be called
	for _, m := range []string{"Vacuum", "Balance", "FixReplication", "Grow", "Execute"} {
		if count := h.weedcli.CallCount(m); count != 0 {
			t.Fatalf("CRITICAL SECURITY VIOLATION: weedcli.%s was invoked %d times while AllowAdminActions=false!", m, count)
		}
	}
}

// ============================================================================
// SUITE 2: S3 Bucket Mutating Guardrail (AllowAdminActions=false)
// ============================================================================

func TestAdversarial_S3BucketMutating_ForbiddenWhenAdminDisabled(t *testing.T) {
	h := setupAdvServer(t, func(c *config.Config) {
		c.AllowAdminActions = false
	})

	t.Run("CreateBucket_PayloadVariants", func(t *testing.T) {
		bucketPayloads := []struct {
			desc string
			body string
		}{
			{"StandardBucket", `{"name":"test-bucket"}`},
			{"HostileTraversal", `{"name":"../../etc/passwd"}`},
			{"HostileSemicolon", `{"name":"bucket; rm -rf /"}`},
			{"HostilePipe", `{"name":"bucket | id"}`},
			{"HostileBackticks", "{\"name\":\"`whoami`\"}"},
			{"EmptyName", `{"name":""}`},
			{"MalformedJSON", `{"name":`},
			{"EmptyBody", ``},
		}

		for _, pl := range bucketPayloads {
			t.Run(pl.desc, func(t *testing.T) {
				url := h.server.URL + "/api/v1/clusters/test-cluster/s3/buckets"
				var r io.Reader
				if pl.body != "" {
					r = strings.NewReader(pl.body)
				}
				req, _ := http.NewRequest(http.MethodPost, url, r)
				if pl.body != "" {
					req.Header.Set("Content-Type", "application/json")
				}

				resp, err := h.client.Do(req)
				if err != nil {
					t.Fatalf("request failed: %v", err)
				}
				defer resp.Body.Close()

				if resp.StatusCode != http.StatusForbidden {
					body, _ := io.ReadAll(resp.Body)
					t.Fatalf("expected 403 Forbidden for create bucket %s, got %d. Body: %s",
						pl.desc, resp.StatusCode, string(body))
				}

				var errResp APIError
				_ = json.NewDecoder(resp.Body).Decode(&errResp)
				if errResp.Code != "ADMIN_ACTIONS_DISABLED" {
					t.Errorf("expected ADMIN_ACTIONS_DISABLED, got %q", errResp.Code)
				}
			})
		}
	})

	t.Run("DeleteBucket_PathVariants", func(t *testing.T) {
		bucketNames := []string{
			"standard-bucket",
			"..%2F..%2Fetc%2Fpasswd",
			"bucket%3Bid",
			"bucket%7Cwhoami",
			"bucket%0Awhoami",
		}

		for _, b := range bucketNames {
			t.Run(b, func(t *testing.T) {
				url := fmt.Sprintf("%s/api/v1/clusters/test-cluster/s3/buckets/%s", h.server.URL, b)
				req, _ := http.NewRequest(http.MethodDelete, url, nil)

				resp, err := h.client.Do(req)
				if err != nil {
					t.Fatalf("request failed: %v", err)
				}
				defer resp.Body.Close()

				if resp.StatusCode != http.StatusForbidden {
					body, _ := io.ReadAll(resp.Body)
					t.Fatalf("expected 403 Forbidden for delete bucket %s, got %d. Body: %s",
						b, resp.StatusCode, string(body))
				}

				var errResp APIError
				_ = json.NewDecoder(resp.Body).Decode(&errResp)
				if errResp.Code != "ADMIN_ACTIONS_DISABLED" {
					t.Errorf("expected ADMIN_ACTIONS_DISABLED, got %q", errResp.Code)
				}
			})
		}
	})

	// CRITICAL SECURITY ASSERTION: Zero CLI calls for bucket creation or deletion
	if count := h.weedcli.CallCount("CreateBucket"); count != 0 {
		t.Fatalf("CRITICAL SECURITY VIOLATION: CreateBucket was invoked %d times while AllowAdminActions=false!", count)
	}
	if count := h.weedcli.CallCount("DeleteBucket"); count != 0 {
		t.Fatalf("CRITICAL SECURITY VIOLATION: DeleteBucket was invoked %d times while AllowAdminActions=false!", count)
	}
}

// ============================================================================
// SUITE 3: Shell Execution Guardrail
// ============================================================================

func TestAdversarial_ShellExecution_StrictGuards(t *testing.T) {
	t.Run("RawShellDisabled_RejectsArbitraryCommands", func(t *testing.T) {
		h := setupAdvServer(t, func(c *config.Config) {
			c.EnableRawShell = false
			c.AllowAdminActions = false
		})

		hostileCommands := []string{
			"whoami",
			"cat /etc/passwd",
			"curl http://169.254.169.254/latest/meta-data/",
			"volume.delete -volumeId=1",
			"volume.grow -count=1",
			"volume.balance",
			"volume.vacuum",
			"s3.bucket.create -name=evil",
			"s3.bucket.delete -name=evil",
			"fs.delete /important",
			"rm -rf /",
			"echo pwned",
			"volume.tier.upload",
			"volume.list; cat /etc/passwd",
			"bash -c 'rm -rf /'",
			"volume.copy -from=1 -to=2",
			"volume.move -volumeId=1",
			"sudo reboot",
		}

		for _, cmd := range hostileCommands {
			t.Run(cmd, func(t *testing.T) {
				reqBody, _ := json.Marshal(map[string]string{"command": cmd})
				req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/api/v1/clusters/test-cluster/shell", bytes.NewReader(reqBody))
				req.Header.Set("Content-Type", "application/json")

				resp, err := h.client.Do(req)
				if err != nil {
					t.Fatalf("request failed: %v", err)
				}
				defer resp.Body.Close()

				if resp.StatusCode != http.StatusForbidden {
					body, _ := io.ReadAll(resp.Body)
					t.Fatalf("expected 403 Forbidden for %q, got %d. Body: %s", cmd, resp.StatusCode, string(body))
				}

				var errResp APIError
				_ = json.NewDecoder(resp.Body).Decode(&errResp)
				if errResp.Code != "RAW_SHELL_DISABLED" {
					t.Errorf("expected RAW_SHELL_DISABLED for %q, got %q", cmd, errResp.Code)
				}
			})
		}
	})

	t.Run("RawShellDisabled_AllowsPresets", func(t *testing.T) {
		h := setupAdvServer(t, func(c *config.Config) {
			c.EnableRawShell = false
			c.AllowAdminActions = false
		})

		presetCommands := []string{
			"volume.list",
			"volume.list -collection=test",
			"cluster.status",
			"s3.bucket.list",
			"s3.configure",
			"fs.ls /",
			"fs.ls /tmp",
			"fs.meta.cat /test.txt",
			"ec.shard.list",
		}

		for _, cmd := range presetCommands {
			t.Run(cmd, func(t *testing.T) {
				reqBody, _ := json.Marshal(map[string]string{"command": cmd})
				req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/api/v1/clusters/test-cluster/shell", bytes.NewReader(reqBody))
				req.Header.Set("Content-Type", "application/json")

				resp, err := h.client.Do(req)
				if err != nil {
					t.Fatalf("request failed: %v", err)
				}
				defer resp.Body.Close()

				if resp.StatusCode != http.StatusOK {
					body, _ := io.ReadAll(resp.Body)
					t.Fatalf("expected 200 OK for safe preset %q, got %d. Body: %s", cmd, resp.StatusCode, string(body))
				}
			})
		}
	})

	t.Run("RawShellEnabled_MutatingCommandsBlockedWhenAdminDisabled", func(t *testing.T) {
		h := setupAdvServer(t, func(c *config.Config) {
			c.EnableRawShell = true
			c.AllowAdminActions = false
		})

		mutatingRawCommands := []string{
			"volume.delete -volumeId=1",
			"volume.vacuum",
			"volume.balance -force",
			"volume.grow -count=1",
			"volume.fix.replication",
			"s3.bucket.delete -name=test",
			"fs.delete /some/file",
		}

		for _, cmd := range mutatingRawCommands {
			t.Run(cmd, func(t *testing.T) {
				reqBody, _ := json.Marshal(map[string]string{"command": cmd})
				req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/api/v1/clusters/test-cluster/shell", bytes.NewReader(reqBody))
				req.Header.Set("Content-Type", "application/json")

				resp, err := h.client.Do(req)
				if err != nil {
					t.Fatalf("request failed: %v", err)
				}
				defer resp.Body.Close()

				if resp.StatusCode != http.StatusForbidden {
					body, _ := io.ReadAll(resp.Body)
					t.Fatalf("expected 403 Forbidden for mutating shell command %q, got %d. Body: %s", cmd, resp.StatusCode, string(body))
				}

				var errResp APIError
				_ = json.NewDecoder(resp.Body).Decode(&errResp)
				if errResp.Code != "ADMIN_ACTIONS_DISABLED" {
					t.Errorf("expected ADMIN_ACTIONS_DISABLED for %q, got %q", cmd, errResp.Code)
				}
			})
		}
	})
}

// ============================================================================
// SUITE 4: Panic Recovery & Crash Prevention
// ============================================================================

func TestAdversarial_PanicRecovery_Resilience(t *testing.T) {
	cfg := config.DefaultConfig()
	srv := NewServer(cfg, nil, nil, nil, nil, web.Content)

	panicScenarios := []struct {
		name      string
		panicFunc func()
	}{
		{"StringPanic", func() { panic("unexpected internal condition") }},
		{"ErrorPanic", func() { panic(errors.New("fatal nil pointer in service")) }},
		{"NilPointerDereference", func() {
			var p *int
			_ = *p
		}},
		{"IntegerDivideByZero", func() {
			zero := 0
			_ = 100 / zero
		}},
		{"SliceIndexOutOfBounds", func() {
			s := []int{1, 2}
			_ = s[10]
		}},
		{"NilMapAssignment", func() {
			var m map[string]int
			m["fail"] = 1
		}},
	}

	for _, sc := range panicScenarios {
		t.Run(sc.name, func(t *testing.T) {
			panickingHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sc.panicFunc()
			})

			// Wrap directly with srv.recoveryMiddleware
			wrapped := srv.recoveryMiddleware(panickingHandler)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "http://localhost/test-panic", nil)
			req.Header.Set("X-Request-ID", "trace-panic-12345")

			// Should never panic out of ServeHTTP
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("FAILED: recovery middleware allowed panic to leak to caller: %v", r)
				}
			}()

			wrapped.ServeHTTP(rec, req)

			resp := rec.Result()
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusInternalServerError {
				t.Fatalf("expected 500 Internal Server Error, got %d", resp.StatusCode)
			}

			// Verify Content-Type
			contentType := resp.Header.Get("Content-Type")
			if !strings.Contains(contentType, "application/json") {
				t.Errorf("expected application/json on panic response, got %q", contentType)
			}

			var errResp APIError
			if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
				t.Fatalf("failed to decode JSON response: %v", err)
			}

			if errResp.Code != "PANIC_RECOVERED" {
				t.Errorf("expected code PANIC_RECOVERED, got %q", errResp.Code)
			}
			if errResp.Error != "internal server error" {
				t.Errorf("expected error 'internal server error', got %q", errResp.Error)
			}
		})
	}

	t.Run("ConcurrentPanicsDoNotAffectHealthyRequests", func(t *testing.T) {
		h := setupAdvServer(t, nil)

		var wg sync.WaitGroup
		totalRequests := 100
		var panicsRecovered atomic.Int64
		var healthySuccesses atomic.Int64

		for i := 0; i < totalRequests; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				if id%2 == 0 {
					// Healthy request through standard server handler
					req := httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil)
					rec := httptest.NewRecorder()
					h.handler.ServeHTTP(rec, req)
					if rec.Code == http.StatusOK {
						healthySuccesses.Add(1)
					}
				} else {
					// Route that panics inside recovery middleware
					rec := httptest.NewRecorder()
					req := httptest.NewRequest(http.MethodGet, "/panic", nil)
					panicky := h.srv.recoveryMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						panic("simulated concurrent chaos")
					}))
					panicky.ServeHTTP(rec, req)
					if rec.Code == http.StatusInternalServerError {
						panicsRecovered.Add(1)
					}
				}
			}(i)
		}

		wg.Wait()

		if healthySuccesses.Load() != 50 {
			t.Errorf("expected 50 healthy successes, got %d", healthySuccesses.Load())
		}
		if panicsRecovered.Load() != 50 {
			t.Errorf("expected 50 recovered panics, got %d", panicsRecovered.Load())
		}
	})
}

// ============================================================================
// SUITE 5: Massive Concurrency & Race Detector Under Heavy Load
// ============================================================================

func TestAdversarial_Concurrency_RaceStress(t *testing.T) {
	h := setupAdvServer(t, func(c *config.Config) {
		c.AllowAdminActions = false
		c.EnableRawShell = false
	})

	workers := 80
	requestsPerWorker := 25
	totalRequests := workers * requestsPerWorker

	var wg sync.WaitGroup
	startBarrier := make(chan struct{})

	endpoints := []struct {
		method       string
		path         string
		body         string
		expectedCode int
	}{
		{http.MethodGet, "/api/v1/healthz", "", http.StatusOK},
		{http.MethodGet, "/api/v1/config", "", http.StatusOK},
		{http.MethodGet, "/api/v1/clusters", "", http.StatusOK},
		{http.MethodGet, "/api/v1/clusters/test-cluster/overview", "", http.StatusOK},
		{http.MethodGet, "/api/v1/clusters/test-cluster/topology", "", http.StatusOK},
		{http.MethodGet, "/api/v1/clusters/test-cluster/volumes", "", http.StatusOK},
		{http.MethodGet, "/api/v1/clusters/test-cluster/filersync", "", http.StatusOK},
		{http.MethodGet, "/api/v1/clusters/test-cluster/s3/buckets", "", http.StatusOK},
		{http.MethodGet, "/api/v1/clusters/test-cluster/s3/users", "", http.StatusOK},
		{http.MethodGet, "/api/v1/clusters/test-cluster/filer?path=/", "", http.StatusOK},
		{http.MethodGet, "/api/v1/clusters/test-cluster/filer/meta?path=/test.txt", "", http.StatusOK},
		{http.MethodPost, "/api/v1/clusters/test-cluster/shell", `{"command":"volume.list"}`, http.StatusOK},
		// Mutating endpoints (MUST be 403 Forbidden under concurrent load)
		{http.MethodPost, "/api/v1/clusters/test-cluster/actions/vacuum", `{"collection":"test"}`, http.StatusForbidden},
		{http.MethodPost, "/api/v1/clusters/test-cluster/actions/balance", `{}`, http.StatusForbidden},
		{http.MethodPost, "/api/v1/clusters/test-cluster/actions/fix.replication", `{}`, http.StatusForbidden},
		{http.MethodPost, "/api/v1/clusters/test-cluster/actions/volume.grow", `{"count":1}`, http.StatusForbidden},
		{http.MethodPost, "/api/v1/clusters/test-cluster/s3/buckets", `{"name":"new-bkt"}`, http.StatusForbidden},
		{http.MethodDelete, "/api/v1/clusters/test-cluster/s3/buckets/del-bkt", "", http.StatusForbidden},
		// SPA static assets
		{http.MethodGet, "/", "", http.StatusOK},
		{http.MethodGet, "/index.html", "", http.StatusOK},
		{http.MethodGet, "/css/styles.css", "", http.StatusOK},
		{http.MethodGet, "/js/app.js", "", http.StatusOK},
		{http.MethodGet, "/overview", "", http.StatusOK},
		{http.MethodGet, "/topology", "", http.StatusOK},
		// Non-existent API route
		{http.MethodGet, "/api/v1/nonexistent", "", http.StatusNotFound},
	}

	var totalSuccess atomic.Int64
	var totalFailures atomic.Int64

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			<-startBarrier // Synchronize all workers to release concurrently

			for i := 0; i < requestsPerWorker; i++ {
				ep := endpoints[(workerID+i)%len(endpoints)]
				var body io.Reader
				if ep.body != "" {
					body = strings.NewReader(ep.body)
				}
				req := httptest.NewRequest(ep.method, ep.path, body)
				if ep.body != "" {
					req.Header.Set("Content-Type", "application/json")
				}
				rec := httptest.NewRecorder()

				h.handler.ServeHTTP(rec, req)

				if rec.Code == ep.expectedCode {
					totalSuccess.Add(1)
				} else {
					totalFailures.Add(1)
					t.Errorf("Worker %d iter %d: %s %s expected %d, got %d",
						workerID, i, ep.method, ep.path, ep.expectedCode, rec.Code)
				}

				// Verify X-Request-ID was injected
				if reqID := rec.Header().Get("X-Request-ID"); reqID == "" {
					t.Errorf("missing X-Request-ID header on %s %s", ep.method, ep.path)
				}
			}
		}(w)
	}

	// Release all workers simultaneously
	close(startBarrier)
	wg.Wait()

	if totalFailures.Load() > 0 {
		t.Fatalf("%d out of %d concurrent requests failed", totalFailures.Load(), totalRequests)
	}

	if totalSuccess.Load() != int64(totalRequests) {
		t.Fatalf("expected %d successes, got %d", totalRequests, totalSuccess.Load())
	}

	// Verify that CLI was NEVER called for mutating actions
	for _, m := range []string{"Vacuum", "Balance", "FixReplication", "Grow", "CreateBucket", "DeleteBucket"} {
		if count := h.weedcli.CallCount(m); count != 0 {
			t.Fatalf("CRITICAL SECURITY VIOLATION: weedcli.%s was invoked %d times during concurrent burst while AllowAdminActions=false!", m, count)
		}
	}
}

// ============================================================================
// SUITE 6: HTTP Method Tampering & Routing Rigidity
// ============================================================================

func TestAdversarial_HTTPMethodTampering(t *testing.T) {
	h := setupAdvServer(t, func(c *config.Config) {
		c.AllowAdminActions = true
		c.EnableRawShell = true
	})

	tamperedRequests := []struct {
		name       string
		method     string
		path       string
		unexpected bool // if true, should not execute mutating logic
	}{
		{"GETOnActionVacuum", http.MethodGet, "/api/v1/clusters/test-cluster/actions/vacuum", true},
		{"PUTOnActionVacuum", http.MethodPut, "/api/v1/clusters/test-cluster/actions/vacuum", true},
		{"DELETEOnActionVacuum", http.MethodDelete, "/api/v1/clusters/test-cluster/actions/vacuum", true},
		{"GETOnShellExecute", http.MethodGet, "/api/v1/clusters/test-cluster/shell", true},
		{"DELETEOnShellExecute", http.MethodDelete, "/api/v1/clusters/test-cluster/shell", true},
		{"PUTOnCreateBucket", http.MethodPut, "/api/v1/clusters/test-cluster/s3/buckets", true},
	}

	for _, tc := range tamperedRequests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()

			h.handler.ServeHTTP(rec, req)

			// Tampered methods against specific endpoints will hit 404 or 405 or fallback
			if tc.unexpected && rec.Code == http.StatusOK {
				t.Fatalf("CRITICAL: Tampered method %s on %s executed successfully with status 200!", tc.method, tc.path)
			}
		})
	}

	// Ensure zero vacuum or shell execution occurred from tampered methods
	if count := h.weedcli.CallCount("Vacuum"); count != 0 {
		t.Fatalf("Vacuum was invoked %d times via tampered HTTP methods!", count)
	}
}
