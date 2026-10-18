package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAdversarial_Config_ExtremeListenAddresses tests various malformed and boundary listen addresses.
func TestAdversarial_Config_ExtremeListenAddresses(t *testing.T) {
	tests := []struct {
		name    string
		addr    string
		wantErr bool
	}{
		{"empty string", "", true},
		{"whitespace only", "   ", true},
		{"missing port", "127.0.0.1", true},
		{"missing port localhost", "localhost", true},
		{"port 0", ":0", true},
		{"port 65536 overflow", ":65536", true},
		{"port 99999 overflow", ":99999", true},
		{"negative port", ":-80", true},
		{"non-numeric port", ":http", true},
		{"multiple colons invalid ipv6 without brackets", "::1:8080", true},
		{"valid brackets ipv6", "[::1]:8080", false},
		{"valid port 1", ":1", false},
		{"valid port 65535", ":65535", false},
		{"valid 0.0.0.0:8080", "0.0.0.0:8080", false},
		{"valid 127.0.0.1:9090", "127.0.0.1:9090", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.ListenAddr = tc.addr
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("expected error for addr %q, got nil", tc.addr)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected valid addr for %q, got error: %v", tc.addr, err)
			}
		})
	}
}

// TestAdversarial_Config_TimeoutsEdgeCases tests boundary values for query and action timeouts.
func TestAdversarial_Config_TimeoutsEdgeCases(t *testing.T) {
	tests := []struct {
		name          string
		queryTimeout  time.Duration
		actionTimeout time.Duration
		wantErr       bool
	}{
		{"zero query timeout", 0, 10 * time.Second, true},
		{"negative query timeout", -1 * time.Second, 10 * time.Second, true},
		{"action timeout less than query timeout", 10 * time.Second, 5 * time.Second, true},
		{"action timeout equal to query timeout", 5 * time.Second, 5 * time.Second, false},
		{"both valid", 5 * time.Second, 30 * time.Second, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.QueryTimeout = tc.queryTimeout
			cfg.ActionTimeout = tc.actionTimeout
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("expected error for query=%v action=%v, got nil", tc.queryTimeout, tc.actionTimeout)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected valid for query=%v action=%v, got error: %v", tc.queryTimeout, tc.actionTimeout, err)
			}
		})
	}
}

// TestAdversarial_Config_ClusterValidation tests malformed cluster configs.
func TestAdversarial_Config_ClusterValidation(t *testing.T) {
	tests := []struct {
		name      string
		cluster   ClusterConfig
		errSubstr string
	}{
		{
			name: "empty id",
			cluster: ClusterConfig{
				ID:      "",
				Masters: []string{"localhost:9333"},
			},
			errSubstr: "cluster id cannot be empty",
		},
		{
			name: "whitespace id",
			cluster: ClusterConfig{
				ID:      "   ",
				Masters: []string{"localhost:9333"},
			},
			errSubstr: "cluster id cannot be empty",
		},
		{
			name: "invalid id characters space",
			cluster: ClusterConfig{
				ID:      "my cluster",
				Masters: []string{"localhost:9333"},
			},
			errSubstr: "cluster id must contain only alphanumeric",
		},
		{
			name: "invalid id characters special symbols",
			cluster: ClusterConfig{
				ID:      "cluster@prod#1",
				Masters: []string{"localhost:9333"},
			},
			errSubstr: "cluster id must contain only alphanumeric",
		},
		{
			name: "empty masters list",
			cluster: ClusterConfig{
				ID:      "valid-id",
				Masters: []string{},
			},
			errSubstr: "cluster masters list cannot be empty",
		},
		{
			name: "master missing port",
			cluster: ClusterConfig{
				ID:      "valid-id",
				Masters: []string{"192.168.1.1"},
			},
			errSubstr: "cluster master address must be a valid host:port",
		},
		{
			name: "filer missing port",
			cluster: ClusterConfig{
				ID:      "valid-id",
				Masters: []string{"192.168.1.1:9333"},
				Filer:   "filer-service",
			},
			errSubstr: "endpoint address must be a valid host:port",
		},
		{
			name: "s3 missing port",
			cluster: ClusterConfig{
				ID:      "valid-id",
				Masters: []string{"192.168.1.1:9333"},
				S3:      "s3-service",
			},
			errSubstr: "endpoint address must be a valid host:port",
		},
		{
			name: "cluster TLS cert without key",
			cluster: ClusterConfig{
				ID:      "valid-id",
				Masters: []string{"192.168.1.1:9333"},
				TLS: &TLSConfig{
					Enabled:  true,
					CertFile: "/certs/cert.pem",
				},
			},
			errSubstr: "both certFile and keyFile must be specified",
		},
		{
			name: "filerSync missing ID",
			cluster: ClusterConfig{
				ID:      "valid-id",
				Masters: []string{"192.168.1.1:9333"},
				FilerSyncs: []StaticFilerSyncConfig{
					{
						ID:          "",
						SourceFiler: "f1:8888",
						TargetFiler: "f2:8888",
					},
				},
			},
			errSubstr: "id cannot be empty",
		},
		{
			name: "filerSync missing endpoints",
			cluster: ClusterConfig{
				ID:      "valid-id",
				Masters: []string{"192.168.1.1:9333"},
				FilerSyncs: []StaticFilerSyncConfig{
					{
						ID:          "sync-1",
						SourceFiler: "",
						TargetFiler: "f2:8888",
					},
				},
			},
			errSubstr: "sourceFiler and targetFiler cannot be empty",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Clusters = []ClusterConfig{tc.cluster}
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.errSubstr)
			}
			if !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("expected error %q to contain %q", err.Error(), tc.errSubstr)
			}
		})
	}
}

// TestAdversarial_Config_ConcurrentLoad verifies thread safety of loading configuration concurrently.
func TestAdversarial_Config_ConcurrentLoad(t *testing.T) {
	tempDir := t.TempDir()
	yamlPath := filepath.Join(tempDir, "config.yaml")

	yamlContent := `
listenAddr: ":9999"
allowAdminActions: true
enableRawShell: false
queryTimeout: 20s
actionTimeout: 60s
weedBinaryPath: "/usr/local/bin/weed"
tls:
  enabled: true
  caFile: "/tmp/ca.crt"
  certFile: "/tmp/tls.crt"
  keyFile: "/tmp/tls.key"
clusters:
  - id: "cluster-1"
    name: "Production Cluster"
    masters:
      - "master1:9333"
      - "master2:9333"
`
	if err := os.WriteFile(yamlPath, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			args := []string{"-query-timeout", "25s"}
			cfg, err := LoadConfigWithArgs(yamlPath, args)
			if err != nil {
				t.Errorf("goroutine %d failed to load config: %v", idx, err)
				return
			}
			if cfg.ListenAddr != ":9999" {
				t.Errorf("goroutine %d: unexpected listenAddr: %s", idx, cfg.ListenAddr)
			}
			if cfg.QueryTimeout != 25*time.Second {
				t.Errorf("goroutine %d: unexpected queryTimeout: %v", idx, cfg.QueryTimeout)
			}
		}(i)
	}
	wg.Wait()
}
