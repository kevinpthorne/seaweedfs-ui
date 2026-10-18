# Original User Request

## 2026-10-17T17:15:56Z

Build the production-ready SeaweedFS In-Cluster Admin Web UI (`seaweedfs-ui`), container build (`Dockerfile`), and Helm chart (`charts/seaweedfs-ui`) according to the technical specification in `/Users/kevint/dev/seaweedfs-ui/SPEC.md`.

Working directory: /Users/kevint/dev/seaweedfs-ui
Integrity mode: demo

Reference specification: `/Users/kevint/dev/seaweedfs-ui/SPEC.md`

## Requirements

### R1. Core Configuration, PKI/mTLS Wiring & Hybrid Cluster Discovery
Implement the configuration loader (`internal/config`), PKI/TLS manager (`internal/pki`), and cluster discovery registry (`internal/cluster`) defined in `SPEC.md`:
- Load YAML configuration with environment variable overrides (`ALLOW_ADMIN_ACTIONS`, `ENABLE_RAW_SHELL`, etc.).
- Build Go `*tls.Config` instances with custom Root CA bundles (`ca.crt`) and client X.509 certificates (`tls.crt`, `tls.key`) for mutual TLS (mTLS) over HTTPS.
- Automatically generate a valid `.seaweedfs/security.toml` (`[grpc]`, `[grpc.client]`, `[https.client]`) in `$HOME/.seaweedfs/security.toml` (`/tmp/seaweedfs/.seaweedfs/security.toml`) so `weed shell` gRPC connections use mTLS inside a `readOnlyRootFilesystem: true` container.
- Provide a `CompositeRegistry` that merges static clusters from `config.yaml` (`StaticProvider`) with dynamically discovered in-cluster SeaweedFS Services and `filer.sync` Pods (`K8sDiscoveryProvider`), giving static entries precedence on ID/endpoint collisions while enriching missing endpoints.

### R2. Data Plane Clients (`weedcli`, `weedhttp`, `filersync`) & Safety Guardrails
Implement the three data-plane packages specified in `SPEC.md`:
- **`internal/weedcli`**: Execute commands against `/usr/local/bin/weed shell` (configurable binary path) via `stdin`/`stdout` pipes without shell interpolation, wrapping mutating operations (`vacuum`, `balance`, `fix.replication`, `volume.grow`) in `lock`/`unlock` blocks and enforcing configurable query/action timeouts. Enforce `allowAdminActions` (default `false`) and `enableRawShell` (default `false`), rejecting unauthorized mutating or raw commands. Parse `weed shell` outputs for `volume.list`, `s3.bucket.list`, `s3.configure`, and `fs.meta.cat`.
- **`internal/weedhttp`**: Query SeaweedFS Master (`/cluster/status`, `/dir/status`, `/vol/status`) and Filer directory listings (`GET <path>?pretty=y&limit=200` with `Accept: application/json`) over HTTP/HTTPS using the cluster's `*tls.Config`.
- **`internal/filersync`**: Observe running `weed filer.sync` operations by (1) discovering `filer.sync` pods/targets and parsing command-line flags (`-a`, `-b`, `-a.path`, `-b.path`, `-isActivePassive`, `-metricsPort`), (2) scraping Prometheus `/metrics` endpoints for synced file counts, throughput, error counts, goroutines, and memory, and (3) inspecting Filer sync checkpoint metadata to compute real-time `replicationLagSeconds`.

### R3. REST API & Embedded Single-Page Web UI (`internal/api`, `web/`, `cmd/seaweedfs-ui`)
Implement the HTTP server and embedded Vanilla ES Module + CSS Web UI (`//go:embed`) serving all `/api/v1/*` endpoints and the 6 UI views specified in `SPEC.md`:
1. **Top Bar & Overview**: Cluster selector (with `K8s`/`Static` and `TLS` badges), `READ-ONLY` vs `ADMIN WRITE ENABLED` badge, auto-refresh toggle, Raft quorum/leader status, capacity, and volume slot cards.
2. **Topology & Volumes**: Collapsible `DataCenter -> Rack -> DataNode -> Volume` view, filters (collection, replication, read-only, high garbage), and maintenance action buttons (`Vacuum`, `Balance`, `Fix Replication`, `Grow`) properly gated by `allowAdminActions`.
3. **Filer Sync Monitor**: Directional `Source Filer -> Target Filer` cards displaying mode (`Active-Active` / `Active-Passive`), pod health/restarts/uptime, colour-coded replication lag badge, throughput (`events/sec`), synced files total, and errors.
4. **S3 Buckets & IAM**: Bucket list, create/delete workflows (gated by `allowAdminActions`), and S3 IAM user/policy table.
5. **Filer Browser**: Breadcrumb directory explorer with file sizes/chunks/MIME types and a metadata inspection drawer (`fs.meta.cat`).
6. **Weed Shell Console**: Curated read-only preset buttons, terminal output viewer with duration, and raw command input gated by `enableRawShell`.

### R4. Container & Helm Chart (`Dockerfile`, `charts/seaweedfs-ui/`)
Implement the multi-stage `Dockerfile` and the `charts/seaweedfs-ui` Helm chart as specified in `SPEC.md`:
- `Dockerfile` extracts `/usr/bin/weed` from `chrislusf/seaweedfs`, compiles `seaweedfs-ui` with `CGO_ENABLED=0`, and runs as non-root `65532:65532` with `HOME=/tmp/seaweedfs`.
- Helm chart templates (`serviceaccount.yaml`, `rbac.yaml`, `configmap.yaml`, `deployment.yaml`, `service.yaml`, `ingress.yaml`, `_helpers.tpl`, `values.yaml`, `Chart.yaml`) support conditional read-only RBAC (`Role` vs `ClusterRole` vs disabled), PKI Secret mounting (`tls.existingSecret`), memory-backed `emptyDir` at `/tmp/seaweedfs`, and hardened `securityContext` (`readOnlyRootFilesystem: true`, `runAsNonRoot: true`, `drop: ["ALL"]`).

## Acceptance Criteria

### Build & Static Analysis
- [ ] `go build ./...` and `go vet ./...` succeed with zero warnings or errors, producing a working `seaweedfs-ui` binary with embedded web assets.
- [ ] `go test -v -race ./...` passes 100% of unit and end-to-end integration tests with zero race conditions.

### PKI / mTLS & Security Guardrails Verification
- [ ] Automated test generates an ephemeral Root CA, server certificate, and client certificate, starts a mock HTTPS mTLS SeaweedFS Master/Filer/Metrics server requiring client certificate authentication (`tls.RequireAndVerifyClientCert`), and verifies that `weedhttp` and `filersync` connect and fetch data over mTLS while untrusted clients are rejected.
- [ ] Automated test verifies `EnsureSecurityToml` writes a valid `.seaweedfs/security.toml` containing matching `[grpc]`, `[grpc.client]`, and `[https.client]` certificate paths and that `weedcli` passes `HOME` so `weed shell` loads it.
- [ ] Automated API tests verify that when `allowAdminActions=false`, `POST /api/v1/clusters/{id}/actions/*`, `POST /api/v1/clusters/{id}/s3/buckets`, and `DELETE /api/v1/clusters/{id}/s3/buckets/{bucket}` return `403 Forbidden` and never invoke `weed shell`; and when `allowAdminActions=true`, mutating maintenance commands are wrapped in `lock` / `unlock` in `stdin`.

### End-to-End Mock Harness Verification (CLI + K8s Discovery + `filer.sync` + API + UI)
- [ ] An end-to-end integration test uses a fake/mock `weed` executable script/binary and mock HTTP servers (simulating K8s `/api/v1/services` and `/api/v1/pods`, SeaweedFS Master/Filer, and `filer.sync` `/metrics`) to verify every `/api/v1/*` route returns valid JSON matching `SPEC.md`:
  - `GET /api/v1/healthz` and `GET /api/v1/config`
  - `GET /api/v1/clusters` (verifying merge of static + K8s-discovered clusters)
  - `GET /api/v1/clusters/{id}/overview`, `/topology`, and `/volumes`
  - `GET /api/v1/clusters/{id}/filersync` (verifying parsed `-a`/`-b` flags, scraped Prometheus metrics, and calculated `replicationLagSeconds`)
  - `GET /api/v1/clusters/{id}/s3/buckets`, `POST`, `DELETE`, and `GET /s3/users`
  - `GET /api/v1/clusters/{id}/filer`, `GET /filer/meta`, and `POST /shell`
- [ ] Automated verification confirms `GET /` serves `index.html` and all referenced JS/CSS modules (`/js/app.js`, `/js/api.js`, `/js/views/*.js`, `/css/styles.css`) with HTTP `200 OK` and non-empty valid content covering all 6 views.

### Container & Helm Chart Verification
- [ ] Automated test or verification script validates `charts/seaweedfs-ui` across 3 configurations (`default`, `rbac.create=false`, and `rbac.clusterWide=false` + `config.tls.enabled=true`):
  - If `helm` is installed in the environment, `helm lint charts/seaweedfs-ui` and `helm template` succeed for all 3 configurations.
  - Additionally, a Go test parses and validates the chart templates / rendered YAML structures to confirm `ClusterRole`/`Role` conditional rendering, TLS Secret volume mounts at `/etc/seaweedfs-ui/tls`, `/tmp/seaweedfs` `emptyDir` mount, and `readOnlyRootFilesystem: true`.
