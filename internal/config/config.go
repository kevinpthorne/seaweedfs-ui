package config

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultConfigPath defines the default location for the SeaweedFS UI YAML config.
const DefaultConfigPath = "/etc/seaweedfs-ui/config.yaml"

var clusterIDRegex = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// Sentinel errors returned by Validate()
var (
	ErrEmptyListenAddr      = errors.New("listenAddr cannot be empty")
	ErrInvalidListenAddr    = errors.New("listenAddr must be a valid host:port or :port")
	ErrInvalidQueryTimeout  = errors.New("queryTimeout must be greater than 0")
	ErrInvalidActionTimeout = errors.New("actionTimeout must be greater than or equal to queryTimeout")
	ErrEmptyWeedBinaryPath  = errors.New("weedBinaryPath cannot be empty")
	ErrMismatchedTLSKeyPair = errors.New("both certFile and keyFile must be specified for mTLS")
	ErrEmptyClusterID       = errors.New("cluster id cannot be empty")
	ErrInvalidClusterID     = errors.New("cluster id must contain only alphanumeric characters, dashes, dots, and underscores")
	ErrEmptyMasters         = errors.New("cluster masters list cannot be empty")
	ErrInvalidMasterAddr    = errors.New("cluster master address must be a valid host:port")
	ErrInvalidEndpointAddr  = errors.New("endpoint address must be a valid host:port")
)

// Config represents the complete seaweedfs-ui configuration schema.
type Config struct {
	ListenAddr        string          `yaml:"listenAddr" json:"listenAddr"`
	AllowAdminActions bool            `yaml:"allowAdminActions" json:"allowAdminActions"`
	EnableRawShell    bool            `yaml:"enableRawShell" json:"enableRawShell"`
	QueryTimeout      time.Duration   `yaml:"queryTimeout" json:"queryTimeout"`
	ActionTimeout     time.Duration   `yaml:"actionTimeout" json:"actionTimeout"`
	WeedBinaryPath    string          `yaml:"weedBinaryPath" json:"weedBinaryPath"`
	TLS               TLSConfig       `yaml:"tls" json:"tls"`
	Discovery         DiscoveryConfig `yaml:"discovery" json:"discovery"`
	Clusters          []ClusterConfig `yaml:"clusters" json:"clusters"`
}

// TLSConfig defines mutual TLS and CA configuration for HTTP and gRPC planes.
type TLSConfig struct {
	Enabled            bool   `yaml:"enabled" json:"enabled"`
	InsecureSkipVerify bool   `yaml:"insecureSkipVerify" json:"insecureSkipVerify"`
	CAFile             string `yaml:"caFile" json:"caFile"`
	CertFile           string `yaml:"certFile" json:"certFile"`
	KeyFile            string `yaml:"keyFile" json:"keyFile"`
	SecurityTomlPath   string `yaml:"securityTomlPath,omitempty" json:"securityTomlPath,omitempty"`
}

// DiscoveryConfig defines cluster discovery options.
type DiscoveryConfig struct {
	Kubernetes K8sDiscoveryConfig `yaml:"kubernetes" json:"kubernetes"`
}

// K8sDiscoveryConfig defines parameters for in-cluster Kubernetes discovery.
type K8sDiscoveryConfig struct {
	Enabled              bool          `yaml:"enabled" json:"enabled"`
	ClusterWide          bool          `yaml:"clusterWide" json:"clusterWide"`
	Namespaces           []string      `yaml:"namespaces" json:"namespaces"`
	RefreshInterval      time.Duration `yaml:"refreshInterval" json:"refreshInterval"`
	ServiceSelector      string        `yaml:"serviceSelector" json:"serviceSelector"`
	FilerSyncPodSelector string        `yaml:"filerSyncPodSelector" json:"filerSyncPodSelector"`
	KubeconfigPath       string        `yaml:"kubeconfigPath,omitempty" json:"kubeconfigPath,omitempty"`
}

// ClusterConfig represents a static SeaweedFS cluster definition.
type ClusterConfig struct {
	ID         string                  `yaml:"id" json:"id"`
	Name       string                  `yaml:"name" json:"name"`
	Namespace  string                  `yaml:"namespace,omitempty" json:"namespace,omitempty"`
	Masters    []string                `yaml:"masters" json:"masters"`
	Filer      string                  `yaml:"filer,omitempty" json:"filer,omitempty"`
	S3         string                  `yaml:"s3,omitempty" json:"s3,omitempty"`
	TLS        *TLSConfig              `yaml:"tls,omitempty" json:"tls,omitempty"`
	FilerSyncs []StaticFilerSyncConfig `yaml:"filerSyncs,omitempty" json:"filerSyncs,omitempty"`
}

// StaticFilerSyncConfig defines a statically declared filer.sync instance.
type StaticFilerSyncConfig struct {
	ID          string `yaml:"id" json:"id"`
	Name        string `yaml:"name" json:"name"`
	MetricsURL  string `yaml:"metricsUrl" json:"metricsUrl"`
	SourceFiler string `yaml:"sourceFiler" json:"sourceFiler"`
	TargetFiler string `yaml:"targetFiler" json:"targetFiler"`
}

// DefaultConfig returns a Config populated with standard defaults.
func DefaultConfig() *Config {
	return &Config{
		ListenAddr:        ":8080",
		AllowAdminActions: false,
		EnableRawShell:    false,
		QueryTimeout:      15 * time.Second,
		ActionTimeout:     120 * time.Second,
		WeedBinaryPath:    "/usr/bin/weed",
		TLS: TLSConfig{
			Enabled:            false,
			InsecureSkipVerify: false,
			CAFile:             "/etc/seaweedfs-ui/tls/ca.crt",
			CertFile:           "/etc/seaweedfs-ui/tls/tls.crt",
			KeyFile:            "/etc/seaweedfs-ui/tls/tls.key",
			SecurityTomlPath:   "/tmp/seaweedfs/security.toml",
		},
		Discovery: DiscoveryConfig{
			Kubernetes: K8sDiscoveryConfig{
				Enabled:              true,
				ClusterWide:          true,
				Namespaces:           []string{},
				RefreshInterval:      30 * time.Second,
				ServiceSelector:      "app.kubernetes.io/name=seaweedfs",
				FilerSyncPodSelector: "app.kubernetes.io/component=filer-sync",
			},
		},
		Clusters: []ClusterConfig{},
	}
}

// LoadConfig loads configuration from path (or DefaultConfigPath if empty),
// applies environment variable overrides, and validates the configuration.
func LoadConfig(configPath string) (*Config, error) {
	return LoadConfigWithArgs(configPath, nil)
}

// LoadConfigWithArgs loads configuration like LoadConfig, then applies command-line
// flag overrides from args in strict order: Flags > Env Vars > YAML File > Defaults.
func LoadConfigWithArgs(configPath string, args []string) (*Config, error) {
	cfg := DefaultConfig()

	// 1. Determine config path and parse CLI flags
	path := strings.TrimSpace(configPath)
	explicitPath := path != ""

	var flagConfigPath string
	flagSet := flag.NewFlagSet("seaweedfs-ui", flag.ContinueOnError)
	flagSet.StringVar(&flagConfigPath, "config", "", "Path to config file")
	listenFlag := flagSet.String("listen", "", "HTTP listen address")
	allowAdminFlag := flagSet.Bool("allow-admin-actions", false, "Enable mutating admin actions")
	rawShellFlag := flagSet.Bool("enable-raw-shell", false, "Enable raw weed shell console")
	weedBinFlag := flagSet.String("weed-binary", "", "Path to weed binary")
	queryTimeoutFlag := flagSet.Duration("query-timeout", 0, "Query timeout")
	actionTimeoutFlag := flagSet.Duration("action-timeout", 0, "Action timeout")

	if len(args) > 0 {
		if err := flagSet.Parse(args); err != nil {
			return nil, fmt.Errorf("parse flags: %w", err)
		}
		if flagConfigPath != "" {
			path = flagConfigPath
			explicitPath = true
		}
	}

	if path == "" {
		path = DefaultConfigPath
	}

	if explicitPath {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config file %q: %w", path, err)
		}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse config file %q: %w", path, err)
		}
	} else {
		// Non-explicit default path: load if file exists, ignore if absent
		data, err := os.ReadFile(path)
		if err == nil {
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("parse config file %q: %w", path, err)
			}
		}
	}

	// 2. Apply Environment Variables
	if err := applyEnvOverrides(cfg); err != nil {
		return nil, err
	}

	// 3. Apply CLI Flags (only visited flags override)
	if len(args) > 0 {
		visited := make(map[string]bool)
		flagSet.Visit(func(f *flag.Flag) {
			visited[f.Name] = true
		})

		if visited["listen"] && *listenFlag != "" {
			cfg.ListenAddr = *listenFlag
		}
		if visited["allow-admin-actions"] {
			cfg.AllowAdminActions = *allowAdminFlag
		}
		if visited["enable-raw-shell"] {
			cfg.EnableRawShell = *rawShellFlag
		}
		if visited["weed-binary"] && *weedBinFlag != "" {
			cfg.WeedBinaryPath = *weedBinFlag
		}
		if visited["query-timeout"] && *queryTimeoutFlag > 0 {
			cfg.QueryTimeout = *queryTimeoutFlag
		}
		if visited["action-timeout"] && *actionTimeoutFlag > 0 {
			cfg.ActionTimeout = *actionTimeoutFlag
		}
	}

	// 4. Validate
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return cfg, nil
}

func applyEnvOverrides(cfg *Config) error {
	// Listen address: LISTEN_ADDR or PORT
	if v := getEnv("LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
	} else if v := getEnv("PORT"); v != "" {
		if !strings.HasPrefix(v, ":") {
			cfg.ListenAddr = ":" + v
		} else {
			cfg.ListenAddr = v
		}
	}

	// Admin and shell guardrails
	if v := getEnv("ALLOW_ADMIN_ACTIONS"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid boolean value for ALLOW_ADMIN_ACTIONS: %q", v)
		}
		cfg.AllowAdminActions = b
	}
	if v := getEnv("ENABLE_RAW_SHELL"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid boolean value for ENABLE_RAW_SHELL: %q", v)
		}
		cfg.EnableRawShell = b
	}

	// Timeouts
	if v := getEnv("QUERY_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid duration for QUERY_TIMEOUT: %q: %w", v, err)
		}
		cfg.QueryTimeout = d
	}
	if v := getEnv("ACTION_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid duration for ACTION_TIMEOUT: %q: %w", v, err)
		}
		cfg.ActionTimeout = d
	}

	// CLI Binary
	if v := getEnv("WEED_BINARY_PATH"); v != "" {
		cfg.WeedBinaryPath = v
	}

	// TLS Settings
	if v := getEnv("TLS_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid boolean value for TLS_ENABLED: %q", v)
		}
		cfg.TLS.Enabled = b
	}
	if v := getEnv("TLS_INSECURE_SKIP_VERIFY"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid boolean value for TLS_INSECURE_SKIP_VERIFY: %q", v)
		}
		cfg.TLS.InsecureSkipVerify = b
	}
	if v := getEnv("TLS_CA_FILE"); v != "" {
		cfg.TLS.CAFile = v
	}
	if v := getEnv("TLS_CERT_FILE"); v != "" {
		cfg.TLS.CertFile = v
	}
	if v := getEnv("TLS_KEY_FILE"); v != "" {
		cfg.TLS.KeyFile = v
	}
	if v := getEnv("SECURITY_TOML_PATH"); v != "" {
		cfg.TLS.SecurityTomlPath = v
	}

	// Discovery Settings
	if v := getEnv("K8S_DISCOVERY_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid boolean value for K8S_DISCOVERY_ENABLED: %q", v)
		}
		cfg.Discovery.Kubernetes.Enabled = b
	}
	if v := getEnv("K8S_DISCOVERY_CLUSTER_WIDE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid boolean value for K8S_DISCOVERY_CLUSTER_WIDE: %q", v)
		}
		cfg.Discovery.Kubernetes.ClusterWide = b
	}
	if v := getEnv("K8S_SERVICE_SELECTOR"); v != "" {
		cfg.Discovery.Kubernetes.ServiceSelector = v
	}
	if v := getEnv("K8S_FILER_SYNC_POD_SELECTOR"); v != "" {
		cfg.Discovery.Kubernetes.FilerSyncPodSelector = v
	}
	if v := getEnv("K8S_NAMESPACES"); v != "" {
		parts := strings.Split(v, ",")
		var ns []string
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				ns = append(ns, p)
			}
		}
		cfg.Discovery.Kubernetes.Namespaces = ns
	}
	if v := getEnv("K8S_REFRESH_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid duration for K8S_REFRESH_INTERVAL: %q: %w", v, err)
		}
		cfg.Discovery.Kubernetes.RefreshInterval = d
	}
	if v := getEnv("KUBECONFIG"); v != "" {
		cfg.Discovery.Kubernetes.KubeconfigPath = v
	}

	return nil
}

func getEnv(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

// Validate verifies configuration consistency, non-empty values, and valid ranges.
func (c *Config) Validate() error {
	// 1. ListenAddr
	if strings.TrimSpace(c.ListenAddr) == "" {
		return ErrEmptyListenAddr
	}
	if err := validateHostPortOrPort(c.ListenAddr); err != nil {
		return fmt.Errorf("%w: %q (%v)", ErrInvalidListenAddr, c.ListenAddr, err)
	}

	// 2. Timeouts
	if c.QueryTimeout <= 0 {
		return fmt.Errorf("%w: got %v", ErrInvalidQueryTimeout, c.QueryTimeout)
	}
	if c.ActionTimeout < c.QueryTimeout {
		return fmt.Errorf("%w: actionTimeout (%v) < queryTimeout (%v)", ErrInvalidActionTimeout, c.ActionTimeout, c.QueryTimeout)
	}

	// 3. WeedBinaryPath
	if strings.TrimSpace(c.WeedBinaryPath) == "" {
		return ErrEmptyWeedBinaryPath
	}

	// 4. TLS Config
	if c.TLS.Enabled {
		if (c.TLS.CertFile != "" && c.TLS.KeyFile == "") || (c.TLS.CertFile == "" && c.TLS.KeyFile != "") {
			return ErrMismatchedTLSKeyPair
		}
	}

	// 5. Discovery Config
	if c.Discovery.Kubernetes.RefreshInterval <= 0 {
		c.Discovery.Kubernetes.RefreshInterval = 30 * time.Second
	}

	// 6. Static Clusters
	for i, cluster := range c.Clusters {
		if strings.TrimSpace(cluster.ID) == "" {
			return fmt.Errorf("cluster[%d]: %w", i, ErrEmptyClusterID)
		}
		if !clusterIDRegex.MatchString(cluster.ID) {
			return fmt.Errorf("cluster[%d] (%q): %w", i, cluster.ID, ErrInvalidClusterID)
		}
		if len(cluster.Masters) == 0 {
			return fmt.Errorf("cluster[%d] (%q): %w", i, cluster.ID, ErrEmptyMasters)
		}
		for _, m := range cluster.Masters {
			if _, _, err := net.SplitHostPort(m); err != nil {
				return fmt.Errorf("cluster[%d] (%q) master %q: %w", i, cluster.ID, m, ErrInvalidMasterAddr)
			}
		}
		if cluster.Filer != "" {
			if _, _, err := net.SplitHostPort(cluster.Filer); err != nil {
				return fmt.Errorf("cluster[%d] (%q) filer %q: %w", i, cluster.ID, cluster.Filer, ErrInvalidEndpointAddr)
			}
		}
		if cluster.S3 != "" {
			if _, _, err := net.SplitHostPort(cluster.S3); err != nil {
				return fmt.Errorf("cluster[%d] (%q) s3 %q: %w", i, cluster.ID, cluster.S3, ErrInvalidEndpointAddr)
			}
		}
		if cluster.TLS != nil && cluster.TLS.Enabled {
			if (cluster.TLS.CertFile != "" && cluster.TLS.KeyFile == "") || (cluster.TLS.CertFile == "" && cluster.TLS.KeyFile != "") {
				return fmt.Errorf("cluster[%d] (%q): %w", i, cluster.ID, ErrMismatchedTLSKeyPair)
			}
		}
		for j, fs := range cluster.FilerSyncs {
			if strings.TrimSpace(fs.ID) == "" {
				return fmt.Errorf("cluster[%d] filerSync[%d]: id cannot be empty", i, j)
			}
			if strings.TrimSpace(fs.SourceFiler) == "" || strings.TrimSpace(fs.TargetFiler) == "" {
				return fmt.Errorf("cluster[%d] filerSync[%d]: sourceFiler and targetFiler cannot be empty", i, j)
			}
		}
	}

	return nil
}

func validateHostPortOrPort(addr string) error {
	addr = strings.TrimSpace(addr)
	if strings.HasPrefix(addr, ":") {
		portStr := strings.TrimPrefix(addr, ":")
		port, err := strconv.Atoi(portStr)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("invalid port number %q", portStr)
		}
		return nil
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" {
		return fmt.Errorf("empty host")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid port number %q", portStr)
	}
	return nil
}
