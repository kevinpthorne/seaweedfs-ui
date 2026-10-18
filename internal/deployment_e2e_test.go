package internal_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"seaweedfs-ui/internal/api"
	"seaweedfs-ui/internal/cluster"
	"seaweedfs-ui/internal/config"
	"seaweedfs-ui/internal/filersync"
	"seaweedfs-ui/internal/pki"
	"seaweedfs-ui/internal/weedcli"
	"seaweedfs-ui/internal/weedhttp"
	"seaweedfs-ui/web"
)

type noopDeadlineConn struct {
	net.Conn
}

func (c *noopDeadlineConn) SetDeadline(t time.Time) error      { return nil }
func (c *noopDeadlineConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *noopDeadlineConn) SetWriteDeadline(t time.Time) error { return nil }

type pipeAddr struct {
	addr string
}

func (a *pipeAddr) Network() string { return "tcp" }
func (a *pipeAddr) String() string  { return a.addr }

type pipeListener struct {
	conns     chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once
	addr      string
}

func newPipeListener(addr string) *pipeListener {
	return &pipeListener{
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
		addr:   addr,
	}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case <-l.closed:
		return nil, net.ErrClosed
	case c := <-l.conns:
		return c, nil
	}
}

func (l *pipeListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.closed)
	})
	return nil
}

func (l *pipeListener) Addr() net.Addr {
	return &pipeAddr{addr: l.addr}
}

func (l *pipeListener) Dial() (net.Conn, error) {
	select {
	case <-l.closed:
		return nil, net.ErrClosed
	default:
	}
	s, c := net.Pipe()
	select {
	case <-l.closed:
		_ = s.Close()
		_ = c.Close()
		return nil, net.ErrClosed
	case l.conns <- &noopDeadlineConn{s}:
		return &noopDeadlineConn{c}, nil
	}
}

type pipeNetwork struct {
	mu        sync.Mutex
	listeners map[string]*pipeListener
}

func newPipeNetwork() *pipeNetwork {
	return &pipeNetwork{listeners: make(map[string]*pipeListener)}
}

func (n *pipeNetwork) StartTLSServer(addr string, handler http.Handler, tlsCfg *tls.Config) func() {
	l := newPipeListener(addr)
	n.mu.Lock()
	n.listeners[addr] = l
	n.mu.Unlock()

	tlsListener := tls.NewListener(l, tlsCfg)
	srv := &http.Server{Handler: handler, TLSConfig: tlsCfg}
	go func() {
		_ = srv.Serve(tlsListener)
	}()
	return func() {
		_ = srv.Close()
		_ = l.Close()
	}
}

func (n *pipeNetwork) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	n.mu.Lock()
	l, ok := n.listeners[addr]
	n.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("connection refused: %s", addr)
	}
	return l.Dial()
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(wd)
}

func TestDockerfileSpecification(t *testing.T) {
	root := repoRoot(t)
	dockerfilePath := filepath.Join(root, "Dockerfile")
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("failed to read Dockerfile: %v", err)
	}
	content := string(data)

	requiredDirectives := []string{
		"FROM chrislusf/seaweedfs:",
		"AS weed-cli",
		"FROM golang:",
		"CGO_ENABLED=0",
		"FROM alpine:",
		"65532:65532",
		"COPY --from=weed-cli /usr/bin/weed /usr/local/bin/weed",
		"COPY --from=builder /out/seaweedfs-ui /usr/local/bin/seaweedfs-ui",
		"HOME=/tmp/seaweedfs",
		"USER 65532:65532",
		"EXPOSE 8080",
		`ENTRYPOINT ["/usr/local/bin/seaweedfs-ui"]`,
	}

	for _, directive := range requiredDirectives {
		if !strings.Contains(content, directive) {
			t.Errorf("Dockerfile missing required directive %q", directive)
		}
	}
}

func TestHelmChartConfigurations(t *testing.T) {
	root := repoRoot(t)
	chartDir := filepath.Join(root, "charts", "seaweedfs-ui")

	if _, err := os.Stat(filepath.Join(chartDir, "Chart.yaml")); err != nil {
		t.Fatalf("Chart.yaml missing: %v", err)
	}

	helmBin, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm binary not found in PATH; skipping helm CLI execution")
	}

	// 1. Run helm lint
	lintCmd := exec.Command(helmBin, "lint", chartDir)
	if out, err := lintCmd.CombinedOutput(); err != nil {
		t.Fatalf("helm lint failed: %v\n%s", err, string(out))
	}

	testCases := []struct {
		name              string
		setArgs           []string
		expectClusterRole bool
		expectRole        bool
		expectTLSMount    bool
		expectIngress     bool
	}{
		{
			name:              "Default_ClusterWideRBAC",
			setArgs:           nil,
			expectClusterRole: true,
			expectRole:        false,
			expectTLSMount:    false,
			expectIngress:     false,
		},
		{
			name:              "RBACDisabled",
			setArgs:           []string{"--set", "rbac.create=false"},
			expectClusterRole: false,
			expectRole:        false,
			expectTLSMount:    false,
			expectIngress:     false,
		},
		{
			name: "NamespaceRole_WithTLS_AndIngress",
			setArgs: []string{
				"--set", "rbac.clusterWide=false",
				"--set", "config.tls.enabled=true",
				"--set", "config.tls.existingSecret=seaweedfs-client-tls",
				"--set", "ingress.enabled=true",
			},
			expectClusterRole: false,
			expectRole:        true,
			expectTLSMount:    true,
			expectIngress:     true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"template", "test-release", chartDir, "--namespace", "seaweedfs"}, tc.setArgs...)
			cmd := exec.Command(helmBin, args...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("helm template failed: %v\n%s", err, string(out))
			}

			docs := strings.Split(string(out), "\n---")
			kinds := make(map[string]map[string]any)
			for _, doc := range docs {
				var m map[string]any
				if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
					t.Fatalf("invalid YAML in rendered template: %v", err)
				}
				if kind, ok := m["kind"].(string); ok && kind != "" {
					kinds[kind] = m
				}
			}

			if tc.expectClusterRole {
				if _, ok := kinds["ClusterRole"]; !ok {
					t.Errorf("expected ClusterRole to be rendered")
				}
				if _, ok := kinds["ClusterRoleBinding"]; !ok {
					t.Errorf("expected ClusterRoleBinding to be rendered")
				}
			} else {
				if _, ok := kinds["ClusterRole"]; ok {
					t.Errorf("did not expect ClusterRole to be rendered")
				}
			}

			if tc.expectRole {
				if _, ok := kinds["Role"]; !ok {
					t.Errorf("expected namespace Role to be rendered")
				}
				if _, ok := kinds["RoleBinding"]; !ok {
					t.Errorf("expected namespace RoleBinding to be rendered")
				}
			} else {
				if _, ok := kinds["Role"]; ok {
					t.Errorf("did not expect namespace Role to be rendered")
				}
			}

			if tc.expectIngress {
				if _, ok := kinds["Ingress"]; !ok {
					t.Errorf("expected Ingress to be rendered")
				}
			} else {
				if _, ok := kinds["Ingress"]; ok {
					t.Errorf("did not expect Ingress to be rendered")
				}
			}

			// Validate ConfigMap embedded config.yaml against config.LoadConfig
			cm, ok := kinds["ConfigMap"]
			if !ok {
				t.Fatalf("ConfigMap not rendered")
			}
			dataMap, _ := cm["data"].(map[string]any)
			cfgYAML, _ := dataMap["config.yaml"].(string)
			if cfgYAML == "" {
				t.Fatalf("ConfigMap missing data['config.yaml']")
			}

			tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(tmpCfg, []byte(cfgYAML), 0600); err != nil {
				t.Fatalf("write temp config: %v", err)
			}
			parsedCfg, err := config.LoadConfig(tmpCfg)
			if err != nil {
				t.Fatalf("rendered ConfigMap config.yaml failed config.LoadConfig validation: %v\nYAML:\n%s", err, cfgYAML)
			}
			if parsedCfg.TLS.Enabled != tc.expectTLSMount {
				t.Errorf("expected TLS.Enabled=%v, got %v", tc.expectTLSMount, parsedCfg.TLS.Enabled)
			}

			// Validate Deployment securityContext & volume mounts
			dep, ok := kinds["Deployment"]
			if !ok {
				t.Fatalf("Deployment not rendered")
			}
			depBytes, _ := yaml.Marshal(dep)
			depStr := string(depBytes)

			for _, reqStr := range []string{
				"readOnlyRootFilesystem: true",
				"runAsNonRoot: true",
				"runAsUser: 65532",
				"mountPath: /tmp/seaweedfs",
				"medium: Memory",
			} {
				if !strings.Contains(depStr, reqStr) {
					t.Errorf("Deployment missing expected configuration %q", reqStr)
				}
			}

			if tc.expectTLSMount {
				if !strings.Contains(depStr, "mountPath: /etc/seaweedfs-ui/tls") || !strings.Contains(depStr, "secretName: seaweedfs-client-tls") {
					t.Errorf("expected TLS secret volume and mount in Deployment")
				}
			}
		})
	}
}

func TestEndToEnd_SystemIntegration_WithMTLS(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Generate ephemeral PKI (CA + Server + Client certificates)
	caKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "SeaweedFS Test Root CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	caCert, _ := x509.ParseCertificate(caDER)

	writeKeyPair := func(prefix string, isClient bool) (string, string) {
		key, _ := rsa.GenerateKey(rand.Reader, 2048)
		usage := x509.ExtKeyUsageServerAuth
		if isClient {
			usage = x509.ExtKeyUsageClientAuth
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(2),
			Subject:      pkix.Name{CommonName: prefix},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage:  []x509.ExtKeyUsage{usage},
			IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
			DNSNames:     []string{"localhost", "seaweedfs-master.local"},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatalf("create cert %s: %v", prefix, err)
		}
		certPath := filepath.Join(tmpDir, prefix+".crt")
		keyPath := filepath.Join(tmpDir, prefix+".key")
		_ = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600)
		_ = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600)
		return certPath, keyPath
	}

	caPath := filepath.Join(tmpDir, "ca.crt")
	_ = os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0600)
	srvCertPath, srvKeyPath := writeKeyPair("server", false)
	cliCertPath, cliKeyPath := writeKeyPair("client", true)

	// 2. Verify EnsureSecurityToml generates valid security.toml under HOME
	homeDir := filepath.Join(tmpDir, "home")
	t.Setenv("HOME", homeDir)
	tlsCfg := config.TLSConfig{
		Enabled:          true,
		CAFile:           caPath,
		CertFile:         cliCertPath,
		KeyFile:          cliKeyPath,
		SecurityTomlPath: filepath.Join(homeDir, ".seaweedfs", "security.toml"),
	}
	secTomlPath, err := pki.EnsureSecurityToml(tlsCfg)
	if err != nil {
		t.Fatalf("EnsureSecurityToml failed: %v", err)
	}
	secTomlBytes, err := os.ReadFile(secTomlPath)
	if err != nil {
		t.Fatalf("read security.toml: %v", err)
	}
	for _, section := range []string{"[grpc]", "[grpc.client]", "[https.client]", cliCertPath, cliKeyPath, caPath} {
		if !strings.Contains(string(secTomlBytes), section) {
			t.Errorf("security.toml missing %q", section)
		}
	}

	// 3. Create fake `weed` CLI executable script
	weedBinPath := filepath.Join(tmpDir, "mock-weed.sh")
	weedScript := `#!/bin/sh
input=$(cat)
case "$input" in
  *s3.bucket.list*)
    echo "  photos"
    echo "  backups"
    ;;
  *s3.configure*)
    echo "  user: admin actions: [Admin,Read,Write]"
    ;;
  *fs.meta.cat*)
    echo '{"Name":"report.pdf","Mode":420,"Chunks":[{"file_id":"3,01637037d6","size":4096}]}'
    ;;
  *volume.vacuum*|*volume.balance*|*volume.fix.replication*|*volume.grow*|*s3.bucket.create*|*s3.bucket.delete*)
    echo "OK executed: $input"
    ;;
  *)
    echo "shell output: $input"
    ;;
esac
`
	if err := os.WriteFile(weedBinPath, []byte(weedScript), 0755); err != nil {
		t.Fatalf("write mock weed script: %v", err)
	}

	// 4. Start in-memory mTLS Server simulating SeaweedFS Master + Filer + FilerSync Metrics
	srvCert, _ := tls.LoadX509KeyPair(srvCertPath, srvKeyPath)
	caPool := x509.NewCertPool()
	caPool.AddCert(caCert)

	mux := http.NewServeMux()
	mux.HandleFunc("/cluster/status", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"IsLeader": true,
			"Leader":   r.Host,
			"Peers":    []string{r.Host},
		})
	})
	mux.HandleFunc("/dir/status", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Topology": map[string]any{
				"Max":  100,
				"Free": 85,
				"DataCenters": []map[string]any{
					{
						"Id": "dc1",
						"Racks": []map[string]any{
							{
								"Id": "rack1",
								"DataNodes": []map[string]any{
									{"Url": "10.0.0.1:8080", "Volumes": 15, "Max": 100},
								},
							},
						},
					},
				},
			},
		})
	})
	mux.HandleFunc("/vol/status", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Version": "3.80",
			"Volumes": []map[string]any{
				{
					"Id":               1,
					"Size":             1048576,
					"Collection":       "photos",
					"ReplicaPlacement": 1,
					"FileCount":        120,
					"DeleteCount":      5,
					"DeletedByteCount": 4096,
					"ReadOnly":         false,
				},
			},
		})
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = fmt.Fprintf(w, "SeaweedFS_filerSync_synced_files 450\nprocess_start_time_seconds %d\ngo_goroutines 24\n", time.Now().Add(-10*time.Minute).Unix())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Path": r.URL.Path,
			"Entries": []map[string]any{
				{"FullPath": "/buckets/report.pdf", "Mode": 0644, "FileSize": 4096, "Mime": "application/pdf"},
			},
		})
	})

	netPipe := newPipeNetwork()
	srvHostPort := "127.0.0.1:9333"
	cleanupTLS := netPipe.StartTLSServer(srvHostPort, mux, &tls.Config{
		Certificates: []tls.Certificate{srvCert},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	})
	defer cleanupTLS()

	// Verify untrusted client is rejected by the mTLS server
	untrustedClient := &http.Client{
		Transport: &http.Transport{
			DialContext:     netPipe.DialContext,
			TLSClientConfig: &tls.Config{RootCAs: caPool},
		},
	}
	if _, err := untrustedClient.Get("https://" + srvHostPort + "/cluster/status"); err == nil {
		t.Fatalf("expected untrusted client without client certificate to be rejected by mTLS server")
	}

	// 5. Build Config & Server in Read-Only mode
	cfg := config.DefaultConfig()
	cfg.AllowAdminActions = false
	cfg.EnableRawShell = false
	cfg.WeedBinaryPath = weedBinPath
	cfg.TLS = tlsCfg
	cfg.Discovery.Kubernetes.Enabled = false
	cfg.Clusters = []config.ClusterConfig{
		{
			ID:      "prod-mtls",
			Name:    "Production mTLS Cluster",
			Masters: []string{srvHostPort},
			Filer:   srvHostPort,
			S3:      srvHostPort,
			TLS:     &tlsCfg,
			FilerSyncs: []config.StaticFilerSyncConfig{
				{
					ID:          "sync-dr",
					Name:        "Primary to DR",
					MetricsURL:  "https://" + srvHostPort + "/metrics",
					SourceFiler: srvHostPort,
					TargetFiler: srvHostPort,
				},
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reg := cluster.NewCompositeRegistry(cfg)
	_ = reg.Start(ctx)
	defer reg.Stop()

	cliClient := weedcli.NewClient(cfg, nil, nil, nil, nil)
	httpClient := weedhttp.NewClient(cfg.QueryTimeout)
	httpClient.SetDialContext(netPipe.DialContext)
	syncInspector := filersync.NewInspector(5*time.Second, 30*time.Second, cfg.QueryTimeout)
	syncInspector.SetDialContext(netPipe.DialContext)

	srv := api.NewServer(cfg, reg, cliClient, httpClient, syncInspector, web.Content, "0.1.0-e2e")
	handler := srv.Handler()

	// Verify all read endpoints and static UI assets succeed
	readRoutes := []string{
		"/api/v1/healthz",
		"/api/v1/config",
		"/api/v1/clusters",
		"/api/v1/clusters/prod-mtls/overview",
		"/api/v1/clusters/prod-mtls/topology",
		"/api/v1/clusters/prod-mtls/volumes",
		"/api/v1/clusters/prod-mtls/filersync",
		"/api/v1/clusters/prod-mtls/s3/buckets",
		"/api/v1/clusters/prod-mtls/s3/users",
		"/api/v1/clusters/prod-mtls/filer?path=/buckets",
		"/api/v1/clusters/prod-mtls/filer/meta?path=/buckets/report.pdf",
		"/",
		"/js/app.js",
		"/js/api.js",
		"/js/views/overview.js",
		"/js/views/topology.js",
		"/js/views/filersync.js",
		"/js/views/s3.js",
		"/js/views/filer.js",
		"/js/views/shell.js",
		"/css/styles.css",
	}

	for _, route := range readRoutes {
		req := httptest.NewRequest(http.MethodGet, route, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s expected 200 OK, got %d: %s", route, rec.Code, rec.Body.String())
		}
		if rec.Body.Len() == 0 {
			t.Errorf("GET %s returned empty body", route)
		}
	}

	// Verify mutating endpoints return 403 Forbidden when allowAdminActions=false
	postJSON := func(path string, payload any) int {
		b, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := postJSON("/api/v1/clusters/prod-mtls/actions/vacuum", map[string]any{}); code != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden for vacuum in read-only mode, got %d", code)
	}
	if code := postJSON("/api/v1/clusters/prod-mtls/s3/buckets", map[string]any{"name": "new-bucket"}); code != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden for create bucket in read-only mode, got %d", code)
	}
}

func TestNixFlakeAndGitHubActionWorkflow(t *testing.T) {
	root := repoRoot(t)

	// 1. Validate flake.nix chained derivations (seaweedfs-ui -> docker-image-amd64 & docker-image-arm64 -> helm-chart)
	flakePath := filepath.Join(root, "flake.nix")
	flakeBytes, err := os.ReadFile(flakePath)
	if err != nil {
		t.Fatalf("failed to read flake.nix: %v", err)
	}
	flakeStr := string(flakeBytes)

	for _, req := range []string{
		"buildGoModule",
		`pname = "seaweedfs-ui"`,
		"dockerTools.buildLayeredImage",
		"seaweedfs-ui-linux-amd64",
		"seaweedfs-ui-linux-arm64",
		"docker-image-amd64",
		"docker-image-arm64",
		`pname = "seaweedfs-ui-helm-chart"`,
		"helm package",
	} {
		if !strings.Contains(flakeStr, req) {
			t.Errorf("flake.nix missing required derivation definition %q", req)
		}
	}

	// 2. Validate .github/workflows/publish.yml
	workflowPath := filepath.Join(root, ".github", "workflows", "publish.yml")
	wfBytes, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("failed to read .github/workflows/publish.yml: %v", err)
	}
	var wfDoc map[string]any
	if err := yaml.Unmarshal(wfBytes, &wfDoc); err != nil {
		t.Fatalf("invalid YAML in .github/workflows/publish.yml: %v", err)
	}

	wfStr := string(wfBytes)
	for _, req := range []string{
		"DeterminateSystems/nix-installer-action",
		"DeterminateSystems/magic-nix-cache-action",
		"cachix/cachix-action",
		"nix build .#seaweedfs-ui",
		"nix build .#docker-image-amd64",
		"nix build .#docker-image-arm64",
		"docker buildx imagetools create",
		"nix build .#helm-chart",
		"oci://",
	} {
		if !strings.Contains(wfStr, req) {
			t.Errorf(".github/workflows/publish.yml missing expected step/command %q", req)
		}
	}

	// Ensure strict ordering in workflow: .#seaweedfs-ui -> .#docker-image-* -> .#helm-chart
	idxExec := strings.Index(wfStr, "nix build .#seaweedfs-ui")
	idxImgAmd64 := strings.Index(wfStr, "nix build .#docker-image-amd64")
	idxImgArm64 := strings.Index(wfStr, "nix build .#docker-image-arm64")
	idxChart := strings.Index(wfStr, "nix build .#helm-chart")
	if !(idxExec >= 0 && idxImgAmd64 > idxExec && idxImgArm64 > idxExec && idxChart > idxImgAmd64 && idxChart > idxImgArm64) {
		t.Errorf("expected sequential ordering (.#seaweedfs-ui -> .#docker-image-amd64/arm64 -> .#helm-chart) in workflow")
	}
}

