package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.ListenAddr != ":8080" {
		t.Errorf("expected default ListenAddr ':8080', got %q", cfg.ListenAddr)
	}
	if cfg.AllowAdminActions != false {
		t.Errorf("expected default AllowAdminActions false, got true")
	}
	if cfg.EnableRawShell != false {
		t.Errorf("expected default EnableRawShell false, got true")
	}
	if cfg.QueryTimeout != 15*time.Second {
		t.Errorf("expected default QueryTimeout 15s, got %v", cfg.QueryTimeout)
	}
	if cfg.ActionTimeout != 120*time.Second {
		t.Errorf("expected default ActionTimeout 120s, got %v", cfg.ActionTimeout)
	}
	if cfg.WeedBinaryPath != "/usr/bin/weed" {
		t.Errorf("expected default WeedBinaryPath '/usr/bin/weed', got %q", cfg.WeedBinaryPath)
	}
	if cfg.TLS.Enabled != false {
		t.Errorf("expected default TLS.Enabled false, got true")
	}
	if cfg.Discovery.Kubernetes.Enabled != true {
		t.Errorf("expected default K8s discovery enabled true, got false")
	}
	if cfg.Discovery.Kubernetes.ClusterWide != true {
		t.Errorf("expected default K8s discovery clusterWide true, got false")
	}
	if cfg.Discovery.Kubernetes.RefreshInterval != 30*time.Second {
		t.Errorf("expected default K8s refreshInterval 30s, got %v", cfg.Discovery.Kubernetes.RefreshInterval)
	}
	if len(cfg.Clusters) != 0 {
		t.Errorf("expected default clusters slice empty, got %d items", len(cfg.Clusters))
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("DefaultConfig() should pass validation, got: %v", err)
	}
}

func TestLoadConfig_FromYAML(t *testing.T) {
	yamlContent := `
listenAddr: ":9090"
allowAdminActions: true
enableRawShell: true
queryTimeout: 20s
actionTimeout: 180s
weedBinaryPath: "/opt/bin/weed"

tls:
  enabled: true
  insecureSkipVerify: true
  caFile: "/custom/ca.crt"
  certFile: "/custom/tls.crt"
  keyFile: "/custom/tls.key"
  securityTomlPath: "/tmp/custom.toml"

discovery:
  kubernetes:
    enabled: true
    clusterWide: false
    namespaces: ["swfs-a", "swfs-b"]
    refreshInterval: 45s
    serviceSelector: "app=swfs"
    filerSyncPodSelector: "app=sync"
    kubeconfigPath: "/root/.kube/config"

clusters:
  - id: "cluster-1"
    name: "Production Cluster"
    masters:
      - "master-1:9333"
      - "master-2:9333"
    filer: "filer-1:8888"
    s3: "s3-1:8333"
    filerSyncs:
      - id: "sync-1"
        name: "Main Sync"
        metricsUrl: "http://metrics:9327/metrics"
        sourceFiler: "filer-1:8888"
        targetFiler: "filer-2:8888"
`
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write test YAML: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.ListenAddr != ":9090" {
		t.Errorf("got ListenAddr %q, want :9090", cfg.ListenAddr)
	}
	if !cfg.AllowAdminActions {
		t.Errorf("got AllowAdminActions false, want true")
	}
	if !cfg.EnableRawShell {
		t.Errorf("got EnableRawShell false, want true")
	}
	if cfg.QueryTimeout != 20*time.Second {
		t.Errorf("got QueryTimeout %v, want 20s", cfg.QueryTimeout)
	}
	if cfg.ActionTimeout != 180*time.Second {
		t.Errorf("got ActionTimeout %v, want 180s", cfg.ActionTimeout)
	}
	if cfg.WeedBinaryPath != "/opt/bin/weed" {
		t.Errorf("got WeedBinaryPath %q, want /opt/bin/weed", cfg.WeedBinaryPath)
	}
	if !cfg.TLS.Enabled || !cfg.TLS.InsecureSkipVerify || cfg.TLS.CAFile != "/custom/ca.crt" {
		t.Errorf("TLS fields mismatch: %+v", cfg.TLS)
	}
	if cfg.Discovery.Kubernetes.ClusterWide != false {
		t.Errorf("got ClusterWide true, want false")
	}
	expectedNS := []string{"swfs-a", "swfs-b"}
	if !reflect.DeepEqual(cfg.Discovery.Kubernetes.Namespaces, expectedNS) {
		t.Errorf("got namespaces %v, want %v", cfg.Discovery.Kubernetes.Namespaces, expectedNS)
	}
	if len(cfg.Clusters) != 1 || cfg.Clusters[0].ID != "cluster-1" {
		t.Fatalf("clusters slice mismatch: %+v", cfg.Clusters)
	}
	if len(cfg.Clusters[0].Masters) != 2 || cfg.Clusters[0].Filer != "filer-1:8888" {
		t.Errorf("cluster details mismatch: %+v", cfg.Clusters[0])
	}
}

func TestLoadConfig_FileNotFound(t *testing.T) {
	_, err := LoadConfig("/nonexistent/file/path/config.yaml")
	if err == nil {
		t.Fatalf("expected error loading non-existent explicit file, got nil")
	}
}

func TestLoadConfig_InvalidYAMLSyntax(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "bad_syntax.yaml")
	if err := os.WriteFile(configPath, []byte("listenAddr: [invalid yaml"), 0644); err != nil {
		t.Fatalf("failed to write test YAML: %v", err)
	}

	_, err := LoadConfig(configPath)
	if err == nil {
		t.Fatalf("expected error parsing malformed YAML, got nil")
	}
}

func TestLoadConfig_EnvVarOverrides(t *testing.T) {
	t.Setenv("LISTEN_ADDR", ":7070")
	t.Setenv("ALLOW_ADMIN_ACTIONS", "true")
	t.Setenv("ENABLE_RAW_SHELL", "true")
	t.Setenv("QUERY_TIMEOUT", "10s")
	t.Setenv("ACTION_TIMEOUT", "60s")
	t.Setenv("WEED_BINARY_PATH", "/usr/custom/weed")
	t.Setenv("TLS_ENABLED", "true")
	t.Setenv("TLS_INSECURE_SKIP_VERIFY", "true")
	t.Setenv("TLS_CA_FILE", "/env/ca.crt")
	t.Setenv("TLS_CERT_FILE", "/env/tls.crt")
	t.Setenv("TLS_KEY_FILE", "/env/tls.key")
	t.Setenv("SECURITY_TOML_PATH", "/env/security.toml")
	t.Setenv("K8S_DISCOVERY_ENABLED", "false")
	t.Setenv("K8S_DISCOVERY_CLUSTER_WIDE", "false")
	t.Setenv("K8S_SERVICE_SELECTOR", "app=swfs-env")
	t.Setenv("K8S_FILER_SYNC_POD_SELECTOR", "app=sync-env")
	t.Setenv("K8S_NAMESPACES", "ns1, ns2,ns3")
	t.Setenv("K8S_REFRESH_INTERVAL", "1m")
	t.Setenv("KUBECONFIG", "/custom/kubeconfig")

	// Call with empty string (loads defaults + env overrides)
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.ListenAddr != ":7070" {
		t.Errorf("got ListenAddr %q, want :7070", cfg.ListenAddr)
	}
	if !cfg.AllowAdminActions {
		t.Errorf("got AllowAdminActions false, want true")
	}
	if !cfg.EnableRawShell {
		t.Errorf("got EnableRawShell false, want true")
	}
	if cfg.QueryTimeout != 10*time.Second {
		t.Errorf("got QueryTimeout %v, want 10s", cfg.QueryTimeout)
	}
	if cfg.ActionTimeout != 60*time.Second {
		t.Errorf("got ActionTimeout %v, want 60s", cfg.ActionTimeout)
	}
	if cfg.WeedBinaryPath != "/usr/custom/weed" {
		t.Errorf("got WeedBinaryPath %q, want /usr/custom/weed", cfg.WeedBinaryPath)
	}
	if !cfg.TLS.Enabled || cfg.TLS.CAFile != "/env/ca.crt" || cfg.TLS.CertFile != "/env/tls.crt" {
		t.Errorf("TLS fields mismatch: %+v", cfg.TLS)
	}
	if cfg.Discovery.Kubernetes.Enabled != false {
		t.Errorf("got K8s discovery enabled true, want false")
	}
	if cfg.Discovery.Kubernetes.ClusterWide != false {
		t.Errorf("got ClusterWide true, want false")
	}
	expectedNS := []string{"ns1", "ns2", "ns3"}
	if !reflect.DeepEqual(cfg.Discovery.Kubernetes.Namespaces, expectedNS) {
		t.Errorf("got namespaces %v, want %v", cfg.Discovery.Kubernetes.Namespaces, expectedNS)
	}
	if cfg.Discovery.Kubernetes.RefreshInterval != time.Minute {
		t.Errorf("got RefreshInterval %v, want 1m", cfg.Discovery.Kubernetes.RefreshInterval)
	}
}

func TestLoadConfig_PortEnvFallback(t *testing.T) {
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("PORT", "3000")

	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.ListenAddr != ":3000" {
		t.Errorf("got ListenAddr %q, want :3000", cfg.ListenAddr)
	}
}

func TestPrecedence_FullHierarchy(t *testing.T) {
	// Base YAML has :9000
	yamlContent := `
listenAddr: ":9000"
allowAdminActions: false
`
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write YAML: %v", err)
	}

	// Step 1: Default only
	cfgDef := DefaultConfig()
	if cfgDef.ListenAddr != ":8080" {
		t.Errorf("Default listenAddr expected :8080, got %q", cfgDef.ListenAddr)
	}

	// Step 2: YAML overrides Default
	cfgYAML, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfgYAML.ListenAddr != ":9000" {
		t.Errorf("YAML should override Default: got %q, want :9000", cfgYAML.ListenAddr)
	}

	// Step 3: Env Var overrides YAML
	t.Setenv("LISTEN_ADDR", ":9090")
	cfgEnv, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfgEnv.ListenAddr != ":9090" {
		t.Errorf("Env Var should override YAML: got %q, want :9090", cfgEnv.ListenAddr)
	}

	// Step 4: CLI Flag overrides Env Var and YAML
	args := []string{"-listen", ":9999", "-allow-admin-actions=true"}
	cfgFlag, err := LoadConfigWithArgs(configPath, args)
	if err != nil {
		t.Fatalf("LoadConfigWithArgs failed: %v", err)
	}
	if cfgFlag.ListenAddr != ":9999" {
		t.Errorf("CLI Flag should override Env Var: got %q, want :9999", cfgFlag.ListenAddr)
	}
	if !cfgFlag.AllowAdminActions {
		t.Errorf("CLI Flag should override YAML: got AllowAdminActions false, want true")
	}
}

func TestLoadConfig_InvalidEnvVars(t *testing.T) {
	testCases := []struct {
		envKey string
		envVal string
	}{
		{"ALLOW_ADMIN_ACTIONS", "not_a_bool"},
		{"ENABLE_RAW_SHELL", "yes_please"},
		{"QUERY_TIMEOUT", "invalid_duration"},
		{"ACTION_TIMEOUT", "bad"},
		{"TLS_ENABLED", "maybe"},
		{"TLS_INSECURE_SKIP_VERIFY", "nope"},
		{"K8S_DISCOVERY_ENABLED", "sometimes"},
		{"K8S_DISCOVERY_CLUSTER_WIDE", "100"},
		{"K8S_REFRESH_INTERVAL", "30seconds"},
	}

	for _, tc := range testCases {
		t.Run(tc.envKey, func(t *testing.T) {
			t.Setenv(tc.envKey, tc.envVal)
			_, err := LoadConfig("")
			if err == nil {
				t.Errorf("expected error for invalid env %s=%q, got nil", tc.envKey, tc.envVal)
			}
		})
	}
}

func TestValidation_ListenAddr(t *testing.T) {
	tests := []struct {
		name    string
		addr    string
		wantErr error
	}{
		{"empty", "", ErrEmptyListenAddr},
		{"whitespace", "   ", ErrEmptyListenAddr},
		{"missing port", "localhost", ErrInvalidListenAddr},
		{"invalid port number", ":99999", ErrInvalidListenAddr},
		{"non-numeric port", ":abc", ErrInvalidListenAddr},
		{"valid :port", ":8080", nil},
		{"valid host:port", "127.0.0.1:8080", nil},
		{"valid 0.0.0.0:port", "0.0.0.0:8080", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.ListenAddr = tt.addr
			err := cfg.Validate()
			if tt.wantErr != nil {
				if err == nil || !errors.Is(err, tt.wantErr) {
					t.Errorf("expected error %v, got %v", tt.wantErr, err)
				}
			} else {
				if err != nil {
					t.Errorf("expected valid listenAddr %q, got error: %v", tt.addr, err)
				}
			}
		})
	}
}

func TestValidation_Timeouts(t *testing.T) {
	cfg := DefaultConfig()

	// QueryTimeout <= 0
	cfg.QueryTimeout = 0
	if err := cfg.Validate(); !errors.Is(err, ErrInvalidQueryTimeout) {
		t.Errorf("expected ErrInvalidQueryTimeout for 0, got %v", err)
	}

	cfg.QueryTimeout = -5 * time.Second
	if err := cfg.Validate(); !errors.Is(err, ErrInvalidQueryTimeout) {
		t.Errorf("expected ErrInvalidQueryTimeout for negative, got %v", err)
	}

	// ActionTimeout < QueryTimeout
	cfg.QueryTimeout = 30 * time.Second
	cfg.ActionTimeout = 10 * time.Second
	if err := cfg.Validate(); !errors.Is(err, ErrInvalidActionTimeout) {
		t.Errorf("expected ErrInvalidActionTimeout when ActionTimeout < QueryTimeout, got %v", err)
	}

	// ActionTimeout == QueryTimeout (allowed)
	cfg.ActionTimeout = 30 * time.Second
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected valid when ActionTimeout == QueryTimeout, got %v", err)
	}
}

func TestValidation_WeedBinaryPath(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WeedBinaryPath = ""
	if err := cfg.Validate(); !errors.Is(err, ErrEmptyWeedBinaryPath) {
		t.Errorf("expected ErrEmptyWeedBinaryPath, got %v", err)
	}

	cfg.WeedBinaryPath = "   "
	if err := cfg.Validate(); !errors.Is(err, ErrEmptyWeedBinaryPath) {
		t.Errorf("expected ErrEmptyWeedBinaryPath for whitespace, got %v", err)
	}
}

func TestValidation_TLSConfig(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TLS.Enabled = true

	// CertFile set without KeyFile
	cfg.TLS.CertFile = "/etc/tls/tls.crt"
	cfg.TLS.KeyFile = ""
	if err := cfg.Validate(); !errors.Is(err, ErrMismatchedTLSKeyPair) {
		t.Errorf("expected ErrMismatchedTLSKeyPair when KeyFile is empty, got %v", err)
	}

	// KeyFile set without CertFile
	cfg.TLS.CertFile = ""
	cfg.TLS.KeyFile = "/etc/tls/tls.key"
	if err := cfg.Validate(); !errors.Is(err, ErrMismatchedTLSKeyPair) {
		t.Errorf("expected ErrMismatchedTLSKeyPair when CertFile is empty, got %v", err)
	}

	// Both set -> valid
	cfg.TLS.CertFile = "/etc/tls/tls.crt"
	cfg.TLS.KeyFile = "/etc/tls/tls.key"
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected valid TLSConfig, got error: %v", err)
	}

	// Disabled TLS ignores mismatched cert/key
	cfg.TLS.Enabled = false
	cfg.TLS.KeyFile = ""
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected valid when TLS is disabled, got error: %v", err)
	}
}

func TestValidation_StaticClusters(t *testing.T) {
	cfg := DefaultConfig()

	// Empty ID
	cfg.Clusters = []ClusterConfig{
		{ID: "", Masters: []string{"master:9333"}},
	}
	if err := cfg.Validate(); !errors.Is(err, ErrEmptyClusterID) {
		t.Errorf("expected ErrEmptyClusterID, got %v", err)
	}

	// Invalid characters in ID
	cfg.Clusters = []ClusterConfig{
		{ID: "cluster with spaces!", Masters: []string{"master:9333"}},
	}
	if err := cfg.Validate(); !errors.Is(err, ErrInvalidClusterID) {
		t.Errorf("expected ErrInvalidClusterID, got %v", err)
	}

	// Empty Masters
	cfg.Clusters = []ClusterConfig{
		{ID: "valid-id", Masters: []string{}},
	}
	if err := cfg.Validate(); !errors.Is(err, ErrEmptyMasters) {
		t.Errorf("expected ErrEmptyMasters, got %v", err)
	}

	// Invalid Master address
	cfg.Clusters = []ClusterConfig{
		{ID: "valid-id", Masters: []string{"invalid_no_port"}},
	}
	if err := cfg.Validate(); !errors.Is(err, ErrInvalidMasterAddr) {
		t.Errorf("expected ErrInvalidMasterAddr, got %v", err)
	}

	// Invalid Filer address
	cfg.Clusters = []ClusterConfig{
		{ID: "valid-id", Masters: []string{"master:9333"}, Filer: "filer_no_port"},
	}
	if err := cfg.Validate(); !errors.Is(err, ErrInvalidEndpointAddr) {
		t.Errorf("expected ErrInvalidEndpointAddr for Filer, got %v", err)
	}

	// Invalid S3 address
	cfg.Clusters = []ClusterConfig{
		{ID: "valid-id", Masters: []string{"master:9333"}, S3: "s3_no_port"},
	}
	if err := cfg.Validate(); !errors.Is(err, ErrInvalidEndpointAddr) {
		t.Errorf("expected ErrInvalidEndpointAddr for S3, got %v", err)
	}

	// Valid static cluster
	cfg.Clusters = []ClusterConfig{
		{
			ID:      "prod-seaweedfs",
			Name:    "Production SeaweedFS",
			Masters: []string{"master-0.swfs:9333", "master-1.swfs:9333"},
			Filer:   "filer.swfs:8888",
			S3:      "s3.swfs:8333",
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected valid static cluster, got error: %v", err)
	}
}

func TestValidation_StaticFilerSyncs(t *testing.T) {
	cfg := DefaultConfig()

	// Missing ID
	cfg.Clusters = []ClusterConfig{
		{
			ID:      "prod",
			Masters: []string{"master:9333"},
			FilerSyncs: []StaticFilerSyncConfig{
				{ID: "", SourceFiler: "f1:8888", TargetFiler: "f2:8888"},
			},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for empty filerSync ID, got nil")
	}

	// Missing SourceFiler or TargetFiler
	cfg.Clusters[0].FilerSyncs = []StaticFilerSyncConfig{
		{ID: "sync-1", SourceFiler: "", TargetFiler: "f2:8888"},
	}
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for empty sourceFiler, got nil")
	}
}
