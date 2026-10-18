# Project: SeaweedFS In-Cluster Admin Web UI (`seaweedfs-ui`)

## Architecture
`seaweedfs-ui` is a lightweight, cloud-native administration console and REST proxy designed for Kubernetes in-cluster deployment alongside SeaweedFS. It features zero heavy external dependencies, relying on the Go standard library (`net/http`, `crypto/tls`, `os/exec`, `embed`), zero-build-step Vanilla ES Modules for the UI, and the official `weed` CLI for authoritative administrative gRPC operations.

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                          SeaweedFS Admin Web UI                             │
│                                                                             │
│  ┌───────────────────────┐             ┌─────────────────────────────────┐  │
│  │   Embedded SPA UI     │   HTTP/1.1  │        REST API Engine          │  │
│  │   (web/ - //go:embed) │ ◄─────────► │         (internal/api)          │  │
│  │  ES Modules + Modern  │             │   Router, Middleware & Guards   │  │
│  │      CSS Variables    │             └────────────────┬────────────────┘  │
│  └───────────────────────┘                              │                   │
│                                                         │                   │
│         ┌───────────────────────────────────────────────┴───────────────┐   │
│         │                                                               │   │
│         ▼                                                               ▼   │
│  ┌─────────────────────────────┐                         ┌──────────────────┤
│  │    Hybrid Discovery Engine  │                         │ Safety Guardrails│
│  │      (internal/cluster)     │                         │ (weedcli & weed- │
│  │  CompositeRegistry:         │                         │  http)           │
│  │  - StaticProvider (YAML)    │                         │ - allowAdminAc-  │
│  │  - K8sDiscoveryProvider     │                         │   tions (403)    │
│  │    (Services, Endpoints,    │                         │ - enableRawShell │
│  │     Pods, FilerSync)        │                         │ - Token state-   │
│  └──────────────┬──────────────┘                         │   machine        │
│                 │                                        │ - Audit logging  │
│                 ▼                                        └─────────┬────────┘
│  ┌─────────────────────────────┐                                   │        │
│  │    Configuration & PKI      │                                   │        │
│  │  (internal/config, pki)     │                                   │        │
│  │  - Env & Flag overrides     │                                   │        │
│  │  - HTTPS *tls.Config        │                                   │        │
│  │  - EnsureSecurityToml       │                                   │        │
│  │    (/tmp/seaweedfs/...)     │                                   │        │
│  └──────────────┬──────────────┘                                   │        │
│                 │                                                  │        │
│                 └───────────────────────┬──────────────────────────┘        │
│                                         │                                   │
│         ┌───────────────────────────────┴───────────────────────────────┐   │
│         │                                                               │   │
│         ▼                                                               ▼   │
│  ┌─────────────────────────────┐                         ┌──────────────────┤
│  │     Data Plane HTTP         │                         │ weed CLI Wrapper │
│  │    (internal/weedhttp)      │                         │(internal/weedcli)│
│  │  - Master status & volumes  │                         │ - weed shell gRPC│
│  │  - Filer directory & files  │                         │ - Stdin batch:   │
│  │  - FilerSync lag & metrics  │                         │   lock/unlock    │
│  │    (internal/filersync)     │                         │ - Stdin parsing  │
│  └──────────────┬──────────────┘                         └─────────┬────────┘
│                 │                                                  │        │
└─────────────────┼──────────────────────────────────────────────────┼────────┘
                  │ HTTPS (mTLS)                                     │ gRPC (mTLS)
                  ▼                                                  ▼
     ┌────────────────────────┐                         ┌───────────────────────┐
     │  SeaweedFS HTTP Ports  │                         │ SeaweedFS gRPC Ports  │
     │  - Master (9333)       │                         │ - Master (19333)      │
     │  - Filer (8888)        │                         │ - Filer (18888)       │
     │  - Volume (8080)       │                         │ - Volume (18080)      │
     │  - FilerSync (9327)    │                         │                       │
     └────────────────────────┘                         └───────────────────────┘
```

## Feature Inventory
Every feature identified from specification mining is assigned to a milestone:

| # | Feature | Description | Milestone | Source |
|---|---------|-------------|-----------|--------|
| 1 | YAML Config Loading | Load config from YAML with fallback defaults | M1 | SPEC §4.1, ORIGINAL_REQUEST §R1 |
| 2 | Environment Overrides | Env vars (`ALLOW_ADMIN_ACTIONS`, `ENABLE_RAW_SHELL`, etc.) override YAML | M1 | SPEC §4.1, ORIGINAL_REQUEST §R1 |
| 3 | CLI Flags Parsing | Command-line flags override env and file configs | M1 | SPEC §4.8, ORIGINAL_REQUEST §R1 |
| 4 | Config Validation | Strict validation of URLs, timeouts, TLS paths, and master addresses | M1 | SPEC §4.1 |
| 5 | BuildTLSConfig | Generate `*tls.Config` with custom CA and client cert for HTTPS probes | M1 | SPEC §4.2, ORIGINAL_REQUEST §R1 |
| 6 | Client Cert Auth (mTLS) | Client certificate authentication against mTLS-enabled SeaweedFS endpoints | M1 | SPEC §4.2, ORIGINAL_REQUEST §R1 |
| 7 | Dynamic Cert Reloading | Reload certificates on disk modification for cert-manager rotations | M1 | SPEC §4.2 |
| 8 | EnsureSecurityToml | Write valid `security.toml` in `$HOME/.seaweedfs` (`/tmp/seaweedfs`) for `weed shell` | M1 | SPEC §4.2, ORIGINAL_REQUEST §R1 |
| 9 | Writable tmpfs Target | Support `readOnlyRootFilesystem: true` by using `/tmp/seaweedfs` | M1 | SPEC §4.2, SPEC §6.3 |
| 10 | ClusterTarget Model | Data structure representing SeaweedFS cluster target endpoints | M1 | SPEC §4.3 |
| 11 | StaticProvider | Load static cluster definitions from configuration | M1 | SPEC §4.3, ORIGINAL_REQUEST §R1 |
| 12 | K8s In-Cluster Auth | Read in-cluster ServiceAccount token and CA cert | M1 | SPEC §4.3, ORIGINAL_REQUEST §R1 |
| 13 | K8s Service Discovery | Discover SeaweedFS services by label selector across namespaces | M1 | SPEC §4.3, ORIGINAL_REQUEST §R1 |
| 14 | Port Identification | Map ports (9333 Master, 8888 Filer, 8333 S3) from K8s service specs | M1 | SPEC §4.3 |
| 15 | Headless Service Resolution | Resolve headless services (`clusterIP: None`) via Endpoints/Pods API | M1 | SPEC §4.3 |
| 16 | FilerSync Pod Discovery | Discover `filer.sync` pods and extract CLI arguments for sync pairs | M1 | SPEC §4.3, SPEC §4.6 |
| 17 | CompositeRegistry | Merge static and K8s clusters with static precedence & enrichment | M1 | SPEC §4.3, ORIGINAL_REQUEST §R1 |
| 18 | Master Leader Tracking | Detect active Master leader via `/cluster/status` or `cluster.status` | M1 | SPEC §4.3, SPEC §4.5 |
| 19 | Graceful Non-K8s Fallback | Seamless degradation to static-only mode when running outside Kubernetes | M1 | SPEC §4.3 |
| 20 | weedcli Command Runner | Execute `weed shell` commands with context timeout and output capture | M2 | SPEC §4.4, ORIGINAL_REQUEST §R2 |
| 21 | Stdin Batch Framing | Format stdin with `lock` ... commands ... `unlock` ... `exit` | M2 | SPEC §4.4, ORIGINAL_REQUEST §R2 |
| 22 | Argument Sanitization | Strict regex validation (`^[a-zA-Z0-9._/-]+$`) preventing command injection | M2 | SPEC §4.4 |
| 23 | In-Band Error Detection | Parse stdout/stderr for shell error messages and propagate failure | M2 | SPEC §4.4 |
| 24 | Maintenance: Volume Balance | Execute `volume.balance` via weed shell | M2 | SPEC §4.4, ORIGINAL_REQUEST §R2 |
| 25 | Maintenance: Fix Replication | Execute `volume.fix.replication` via weed shell | M2 | SPEC §4.4, ORIGINAL_REQUEST §R2 |
| 26 | Maintenance: Vacuum | Execute `volume.vacuum` via weed shell | M2 | SPEC §4.4, ORIGINAL_REQUEST §R2 |
| 27 | Maintenance: Volume Grow | Execute `volume.grow` via weed shell | M2 | SPEC §4.4 |
| 28 | S3 Bucket Management | Execute `s3.bucket.list`, `s3.bucket.create`, `s3.bucket.delete` | M2 | SPEC §4.4, ORIGINAL_REQUEST §R2 |
| 29 | S3 IAM User Management | Execute `s3.configure` to list/manage identities and policies | M2 | SPEC §4.4 |
| 30 | Filer Meta Inspection | Execute `fs.meta.cat` to inspect inode metadata | M2 | SPEC §4.4 |
| 31 | EC Operations | Inspect and execute EC operations (`ec.encode`, `ec.rebuild`, `ec.shard.list`) | M2 | SPEC §4.4 |
| 32 | weedhttp Master Probes | HTTP query `/cluster/status`, `/dir/status`, `/vol/status` | M2 | SPEC §4.5, ORIGINAL_REQUEST §R2 |
| 33 | Filer Directory Listing | HTTP browse directory with `Accept: application/json` and `limit=200` | M2 | SPEC §4.5, ORIGINAL_REQUEST §R2 |
| 34 | POSIX Mode Parsing | Interpret POSIX directory flag (`Mode & (1 << 31)`) and file permissions | M2 | SPEC §4.5 |
| 35 | File Streaming Operations | Stream file downloads, chunk metadata inspection, file deletions | M2 | SPEC §4.5 |
| 36 | FilerSync Metric Scraping | Scrape Prometheus `/metrics` (e.g. `sync_offset`, counters) | M2 | SPEC §4.6, ORIGINAL_REQUEST §R2 |
| 37 | FilerSync Lag Calculation | Compute replication lag in seconds: `max(0, now - lastSyncedAt)` | M2 | SPEC §4.6, ORIGINAL_REQUEST §R2 |
| 38 | Safety: Gated Operations | Gate mutating operations on `allowAdminActions` (return 403 Forbidden) | M2 | SPEC §4.4, ORIGINAL_REQUEST §R2 |
| 39 | Safety: Raw Shell Guard | Gate non-allowlisted shell commands on `enableRawShell` (return 403) | M2 | SPEC §4.4, ORIGINAL_REQUEST §R2 |
| 40 | Safety: Action Mutex | In-process per-cluster mutex (`ClusterActionMutex`) preventing concurrent maintenance conflicts | M2 | SPEC §4.4 |
| 41 | Safety: Structured Audit Log | Structured JSON audit log (timestamp, client IP, cluster, action, user, duration, result) | M2 | SPEC §4.4, ORIGINAL_REQUEST §R2 |
| 42 | REST API Server | Go standard library `net/http.ServeMux` with pattern matching | M3 | SPEC §4.7, ORIGINAL_REQUEST §R3 |
| 43 | REST Middleware Pipeline | Panic recovery, request tracing/ID, CORS, admin action guard, audit logging | M3 | SPEC §4.7 |
| 44 | Standard Error Contract | Unified error format: `{"error": string, "code": string, "details": object}` | M3 | SPEC §4.7 |
| 45 | API: Healthz & Config | `GET /api/v1/healthz`, `GET /api/v1/config` | M3 | SPEC §4.7 |
| 46 | API: Clusters & Overview | `GET /api/v1/clusters`, `GET /api/v1/clusters/{id}/overview` | M3 | SPEC §4.7 |
| 47 | API: Topology & Volumes | `GET /api/v1/clusters/{id}/topology`, `GET /api/v1/clusters/{id}/volumes` | M3 | SPEC §4.7 |
| 48 | API: Maintenance Actions | `POST /api/v1/clusters/{id}/actions/{action}` with admin action gating | M3 | SPEC §4.7 |
| 49 | API: Filer Sync | `GET /api/v1/clusters/{id}/filersync` | M3 | SPEC §4.7 |
| 50 | API: S3 Buckets & Users | `GET/POST/DELETE /api/v1/clusters/{id}/s3/buckets`, `GET /api/v1/clusters/{id}/s3/users` | M3 | SPEC §4.7 |
| 51 | API: Filer Directory & Meta | `GET /api/v1/clusters/{id}/filer`, `GET /api/v1/clusters/{id}/filer/meta` | M3 | SPEC §4.7 |
| 52 | API: Shell Execution | `POST /api/v1/clusters/{id}/shell` with raw shell gating & presets | M3 | SPEC §4.7 |
| 53 | Embedded Web UI Assets | Direct embedding of `web/` assets via `//go:embed` | M3 | SPEC §4.8, ORIGINAL_REQUEST §R3 |
| 54 | SPA Fallback Routing | Fallback to `index.html` on non-API routes with 200 OK | M3 | SPEC §4.8 |
| 55 | Top Navigation Bar | Cluster selector dropdown, read-only/admin badge, auto-refresh | M3 | SPEC §4.8 |
| 56 | UI View: Overview | Master leader, Raft quorum, storage gauges, slot meters | M3 | SPEC §4.8 |
| 57 | UI View: Topology & Volumes | Collapsible DC/Rack/Node tree, volume table, garbage ratio | M3 | SPEC §4.8 |
| 58 | UI View: Filer Sync | Source -> Target directional cards, sync metrics, lag badge | M3 | SPEC §4.8 |
| 59 | UI View: S3 Buckets & IAM | Bucket list, create/delete modal, IAM user policy tables | M3 | SPEC §4.8 |
| 60 | UI View: Filer Browser | Directory explorer, breadcrumb navigation, metadata drawer | M3 | SPEC §4.8 |
| 61 | UI View: Shell Console | Safe query presets, terminal output viewer, raw command console | M3 | SPEC §4.8 |
| 62 | Main Entrypoint | `cmd/seaweedfs-ui/main.go` with signal handling & graceful shutdown | M3 | SPEC §4.8, ORIGINAL_REQUEST §R3 |
| 63 | Multi-Stage Dockerfile | 3-stage build: `weed` binary extractor, Go builder, hardened Alpine runtime | M4 | SPEC §5, ORIGINAL_REQUEST §R4 |
| 64 | Non-Root Container Security | Run as `65532:65532`, `HOME=/tmp/seaweedfs`, `readOnlyRootFilesystem: true` | M4 | SPEC §5, SPEC §6 |
| 65 | Helm Chart Templates | Deployment, Service, ServiceAccount, ConfigMap, Secret, Ingress | M4 | SPEC §6, ORIGINAL_REQUEST §R4 |
| 66 | Helm Conditional RBAC | Role vs ClusterRole for services, endpoints, pods; toggleable via values | M4 | SPEC §6, ORIGINAL_REQUEST §R4 |
| 67 | Helm TLS & Volumes | Mount TLS secret at `/etc/seaweedfs-ui/tls`, memory emptyDir at `/tmp/seaweedfs` | M4 | SPEC §6 |
| 68 | Helm Values Customization | Configurable discovery selectors, admin flags, resource limits, ingress | M4 | SPEC §6 |
| 69 | E2E Mock Servers Harness | Mock HTTP/mTLS Master, Filer, FilerSync, K8s API, and mock weed CLI | E2E | ORIGINAL_REQUEST §Testing |
| 70 | Tier 1: Feature Coverage | >=5 tests per feature covering all isolated capabilities | E2E | Testing Track |
| 71 | Tier 2: Boundary & Corner | >=5 tests per feature testing edge conditions, empty inputs, limits | E2E | Testing Track |
| 72 | Tier 3: Cross-Feature Pairwise | Inter-module interaction tests (Discovery+PKI, weedcli+API, etc.) | E2E | Testing Track |
| 73 | Tier 4: Real-World Scenarios | End-to-end multi-cluster failover, maintenance rebalance, sync monitoring | E2E | Testing Track |
| 74 | Tier 5: Adversarial Hardening | White-box stress testing, race conditions, edge-case probing | M5 | Testing Track / Final |

## Milestones
| # | Name | Scope | Dependencies | Status |
|---|------|-------|-------------|--------|
| M1 | Core Configuration, PKI/mTLS Wiring & Hybrid Cluster Discovery | `internal/config`, `internal/pki`, `internal/cluster`, `go.mod` | None | DONE (All gates passed: Reviewers APPROVE, Challengers APPROVE, Auditor CLEAN) |
| M2 | Data Plane Clients & Safety Guardrails | `internal/weedcli`, `internal/weedhttp`, `internal/filersync` | M1 | DONE (All gates passed: Reviewers APPROVE, Challengers APPROVE, Auditor CLEAN) |
| M3 | REST API & Embedded Single-Page Web UI | `internal/api`, `web/`, `cmd/seaweedfs-ui` | M1, M2 | DONE (All gates passed: Reviewers APPROVE, Challengers APPROVE, Auditor CLEAN) |
| M4 | Container & Helm Chart | `Dockerfile`, `charts/seaweedfs-ui` | M3 | DONE (`helm lint`, `helm template` across 3 configs, and `Dockerfile` verified) |
| E2E | E2E Testing Suite (Tiers 1-4) | `internal/deployment_e2e_test.go`, mock mTLS/CLI/K8s servers | Independent track | DONE |
| M5 | 100% E2E Pass & Tier 5 Adversarial Hardening | Comprehensive verification, race detector, adversarial audit | M1, M2, M3, M4, E2E | DONE (`go test -count=1 -race ./...` 100% pass) |

## Interface Contracts

### 1. `internal/config` ↔ All Packages
```go
package config

import "time"

type Config struct {
	ListenAddr        string           `yaml:"listenAddr"`
	AllowAdminActions bool             `yaml:"allowAdminActions"`
	EnableRawShell    bool             `yaml:"enableRawShell"`
	QueryTimeout      time.Duration    `yaml:"queryTimeout"`
	ActionTimeout     time.Duration    `yaml:"actionTimeout"`
	WeedBinaryPath    string           `yaml:"weedBinaryPath"`
	TLS               TLSConfig        `yaml:"tls"`
	Discovery         DiscoveryConfig  `yaml:"discovery"`
	Clusters          []ClusterConfig  `yaml:"clusters"`
}

type TLSConfig struct {
	Enabled            bool   `yaml:"enabled"`
	CAFile             string `yaml:"caFile"`
	CertFile           string `yaml:"certFile"`
	KeyFile            string `yaml:"keyFile"`
	InsecureSkipVerify bool   `yaml:"insecureSkipVerify"`
}

type DiscoveryConfig struct {
	Kubernetes K8sDiscoveryConfig `yaml:"kubernetes"`
}

type K8sDiscoveryConfig struct {
	Enabled              bool              `yaml:"enabled"`
	ClusterWide          bool              `yaml:"clusterWide"`
	Namespaces           []string          `yaml:"namespaces"`
	ServiceSelector      string            `yaml:"serviceSelector"`
	FilerSyncPodSelector string            `yaml:"filerSyncPodSelector"`
	KubeconfigPath       string            `yaml:"kubeconfigPath"`
}

type ClusterConfig struct {
	ID        string    `yaml:"id"`
	Name      string    `yaml:"name"`
	Masters   []string  `yaml:"masters"`
	Filer     string    `yaml:"filer"`
	S3        string    `yaml:"s3"`
	TLS       TLSConfig `yaml:"tls"`
}

func LoadConfig(path string) (*Config, error)
func (c *Config) Validate() error
```

### 2. `internal/pki` ↔ `internal/weedhttp`, `internal/filersync`, `internal/weedcli`
```go
package pki

import (
	"crypto/tls"
	"seaweedfs-ui/internal/config"
)

// BuildTLSConfig constructs a standard crypto/tls.Config for HTTPS REST probes.
func BuildTLSConfig(cfg config.TLSConfig) (*tls.Config, error)

// EnsureSecurityToml creates the .seaweedfs/security.toml file required by weed shell gRPC.
// It writes to /tmp/seaweedfs/.seaweedfs/security.toml (under HOME=/tmp/seaweedfs).
func EnsureSecurityToml(cfg config.TLSConfig) (string, error)
```

### 3. `internal/cluster` ↔ `internal/api`, `internal/weedhttp`, `internal/weedcli`
```go
package cluster

import (
	"context"
	"seaweedfs-ui/internal/config"
)

type ClusterSource string
const (
	SourceStatic     ClusterSource = "static"
	SourceKubernetes ClusterSource = "kubernetes"
)

type ClusterTarget struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Source    ClusterSource    `json:"source"`
	Namespace string           `json:"namespace,omitempty"`
	Masters   []string         `json:"masters"`
	Filer     string           `json:"filer,omitempty"`
	S3        string           `json:"s3,omitempty"`
	FilerSync []FilerSyncTarget`json:"filerSync,omitempty"`
	TLS       config.TLSConfig `json:"tls"`
}

type FilerSyncTarget struct {
	SourceFiler     string `json:"sourceFiler"`
	TargetFiler     string `json:"targetFiler"`
	SourcePath      string `json:"sourcePath"`
	TargetPath      string `json:"targetPath"`
	IsActivePassive bool   `json:"isActivePassive"`
	MetricsPort     int    `json:"metricsPort"`
	PodName         string `json:"podName"`
	Namespace       string `json:"namespace"`
}

type Registry interface {
	ListClusters(ctx context.Context) ([]ClusterTarget, error)
	GetCluster(ctx context.Context, id string) (*ClusterTarget, error)
	Refresh(ctx context.Context) error
}
```

### 4. `internal/weedcli` ↔ `internal/api`
```go
package weedcli

import (
	"context"
	"seaweedfs-ui/internal/cluster"
)

type ShellResult struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exitCode"`
	DurationMs int64  `json:"durationMs"`
}

type Client interface {
	// Maintenance actions (automatically locked/unlocked)
	Vacuum(ctx context.Context, target cluster.ClusterTarget, collection string) (*ShellResult, error)
	Balance(ctx context.Context, target cluster.ClusterTarget) (*ShellResult, error)
	FixReplication(ctx context.Context, target cluster.ClusterTarget) (*ShellResult, error)
	Grow(ctx context.Context, target cluster.ClusterTarget, count int, collection, replication, dataCenter string) (*ShellResult, error)

	// S3 & IAM management
	ListBuckets(ctx context.Context, target cluster.ClusterTarget) ([]string, error)
	CreateBucket(ctx context.Context, target cluster.ClusterTarget, name string) (*ShellResult, error)
	DeleteBucket(ctx context.Context, target cluster.ClusterTarget, name string) (*ShellResult, error)
	ListUsers(ctx context.Context, target cluster.ClusterTarget) ([]S3User, error)

	// Filer & Metadata inspection
	MetaCat(ctx context.Context, target cluster.ClusterTarget, path string) (string, error)

	// Direct Shell Execution (subject to raw shell gating & presets)
	Execute(ctx context.Context, target cluster.ClusterTarget, command string, isRaw bool) (*ShellResult, error)
}
```

### 5. `internal/weedhttp` ↔ `internal/api`
```go
package weedhttp

import (
	"context"
	"io"
	"seaweedfs-ui/internal/cluster"
)

type Client interface {
	GetClusterStatus(ctx context.Context, target cluster.ClusterTarget) (*ClusterStatus, error)
	GetTopology(ctx context.Context, target cluster.ClusterTarget) (*Topology, error)
	GetVolumes(ctx context.Context, target cluster.ClusterTarget) ([]VolumeInfo, error)
	ListFilerDir(ctx context.Context, target cluster.ClusterTarget, path string, limit int) (*FilerDirResult, error)
	StreamFile(ctx context.Context, target cluster.ClusterTarget, path string) (io.ReadCloser, string, error)
	DeleteEntry(ctx context.Context, target cluster.ClusterTarget, path string, isDir bool) error
}
```

### 6. `internal/filersync` ↔ `internal/api`
```go
package filersync

import (
	"context"
	"seaweedfs-ui/internal/cluster"
)

type SyncStatus struct {
	SourceFiler            string  `json:"sourceFiler"`
	TargetFiler            string  `json:"targetFiler"`
	SourcePath             string  `json:"sourcePath"`
	TargetPath             string  `json:"targetPath"`
	Mode                   string  `json:"mode"`
	PodName                string  `json:"podName"`
	Namespace              string  `json:"namespace"`
	ReplicationLagSeconds  float64 `json:"replicationLagSeconds"`
	SyncedFileCount        int64   `json:"syncedFileCount"`
	BytesPerSecond         float64 `json:"bytesPerSecond"`
	LastSyncTimestamp      int64   `json:"lastSyncTimestamp"`
	Status                 string  `json:"status"`
}

type Inspector interface {
	InspectSyncJobs(ctx context.Context, target cluster.ClusterTarget) ([]SyncStatus, error)
}
```

## Code Layout
```text
/Users/kevint/dev/seaweedfs-ui
├── cmd/
│   └── seaweedfs-ui/
│       └── main.go                  # Main server daemon entrypoint & graceful shutdown
├── internal/
│   ├── config/                      # YAML config parsing, env overrides, validation
│   │   ├── config.go
│   │   └── config_test.go
│   ├── pki/                         # HTTPS tls.Config & .seaweedfs/security.toml generation
│   │   ├── pki.go
│   │   └── pki_test.go
│   ├── cluster/                     # Static & K8s discovery, CompositeRegistry
│   │   ├── registry.go
│   │   ├── static.go
│   │   ├── k8s.go
│   │   └── registry_test.go
│   ├── weedcli/                     # weed shell execution, stdin framing, safety guards
│   │   ├── client.go
│   │   ├── parser.go
│   │   └── client_test.go
│   ├── weedhttp/                    # Master/Filer/Volume HTTP probes, Filer browsing
│   │   ├── client.go
│   │   ├── models.go
│   │   └── client_test.go
│   ├── filersync/                   # Filer sync discovery, prometheus metrics & lag
│   │   ├── inspector.go
│   │   └── inspector_test.go
│   ├── api/                         # REST API router, handlers, middleware & audit logger
│   │   ├── server.go
│   │   ├── routes.go
│   │   ├── middleware.go
│   │   ├── audit.go
│   │   └── server_test.go
│   └── testing/                     # Mock SeaweedFS servers, K8s API mock & test harnesses
│       ├── mock_master.go
│       ├── mock_filer.go
│       ├── mock_filersync.go
│       ├── mock_k8s.go
│       └── mock_cli.go
├── web/                             # Zero-build-step Vanilla ES Modules & CSS variables
│   ├── embed.go                     # //go:embed index.html css/* js/*
│   ├── index.html                   # SPA root shell
│   ├── css/
│   │   └── styles.css               # Clean, responsive styles with CSS variables
│   └── js/
│       ├── app.js                   # Application state, router, auto-refresh
│       ├── api.js                   # REST client wrapper & error toast handling
│       └── views/
│           ├── overview.js          # Raft quorum, leader, capacity gauges
│           ├── topology.js          # DC/Rack/Node tree & volume tables
│           ├── filersync.js         # Active sync pairs & lag monitors
│           ├── s3.js                # S3 buckets & IAM user management
│           ├── filer.js             # Filer directory browser & metadata drawer
│           └── shell.js             # Shell presets & console executor
├── Dockerfile                       # Multi-stage production build (nonroot 65532:65532)
├── charts/
│   └── seaweedfs-ui/                # Production Helm chart
│       ├── Chart.yaml
│       ├── values.yaml
│       └── templates/
│           ├── _helpers.tpl
│           ├── deployment.yaml
│           ├── service.yaml
│           ├── serviceaccount.yaml
│           ├── rbac.yaml
│           ├── configmap.yaml
│           ├── secret.yaml
│           └── ingress.yaml
├── go.mod                           # Go module definition
├── go.sum                           # Dependency checksums
├── PROJECT.md                       # Living project index & contracts
├── ORIGINAL_REQUEST.md              # Authoritative user requirements
└── SPEC.md                          # Technical specification
```
