package cluster_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"seaweedfs-ui/internal/cluster"
	"seaweedfs-ui/internal/config"
)

type mockRoundTripper struct {
	handler http.Handler
}

func (m *mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	m.handler.ServeHTTP(rec, req)
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

func newMockK8sClient(handler http.Handler) (*http.Client, string) {
	baseURL := "https://kubernetes.default.svc"
	client := &http.Client{
		Transport: &mockRoundTripper{handler: handler},
		Timeout:   5 * time.Second,
	}
	return client, baseURL
}

func newStandardMockK8sHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// 1. /api/v1/services (or /api/v1/namespaces/{ns}/services)
		if strings.Contains(r.URL.Path, "/services") {
			resp := map[string]interface{}{
				"items": []map[string]interface{}{
					{
						"metadata": map[string]interface{}{
							"name":      "seaweedfs-master",
							"namespace": "seaweedfs",
							"labels": map[string]string{
								"app.kubernetes.io/name":     "seaweedfs",
								"app.kubernetes.io/instance": "seaweedfs",
							},
						},
						"spec": map[string]interface{}{
							"clusterIP": "None", // Headless Master
							"ports": []map[string]interface{}{
								{"name": "swfs-master", "port": 9333},
							},
						},
					},
					{
						"metadata": map[string]interface{}{
							"name":      "seaweedfs-filer",
							"namespace": "seaweedfs",
							"labels": map[string]string{
								"app.kubernetes.io/name":     "seaweedfs",
								"app.kubernetes.io/instance": "seaweedfs",
							},
						},
						"spec": map[string]interface{}{
							"clusterIP": "10.96.10.20",
							"ports": []map[string]interface{}{
								{"name": "swfs-filer", "port": 8888},
							},
						},
					},
					{
						"metadata": map[string]interface{}{
							"name":      "seaweedfs-s3",
							"namespace": "seaweedfs",
							"labels": map[string]string{
								"app.kubernetes.io/name":     "seaweedfs",
								"app.kubernetes.io/instance": "seaweedfs",
							},
						},
						"spec": map[string]interface{}{
							"clusterIP": "10.96.10.30",
							"ports": []map[string]interface{}{
								{"name": "swfs-s3", "port": 8333},
							},
						},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		// 2. /api/v1/namespaces/seaweedfs/endpoints/seaweedfs-master
		if strings.Contains(r.URL.Path, "/endpoints/seaweedfs-master") {
			resp := map[string]interface{}{
				"subsets": []map[string]interface{}{
					{
						"addresses": []map[string]interface{}{
							{
								"hostname": "seaweedfs-master-0",
								"ip":       "10.244.0.15",
								"targetRef": map[string]string{
									"kind": "Pod",
									"name": "seaweedfs-master-0",
								},
							},
							{
								"hostname": "seaweedfs-master-1",
								"ip":       "10.244.1.22",
								"targetRef": map[string]string{
									"kind": "Pod",
									"name": "seaweedfs-master-1",
								},
							},
						},
						"ports": []map[string]interface{}{
							{"name": "swfs-master", "port": 9333},
						},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		// 3. /api/v1/pods
		if strings.Contains(r.URL.Path, "/pods") {
			resp := map[string]interface{}{
				"items": []map[string]interface{}{
					{
						"metadata": map[string]interface{}{
							"name":      "seaweedfs-filer-sync-68df9-abc",
							"namespace": "seaweedfs",
							"labels": map[string]string{
								"app.kubernetes.io/component": "filer-sync",
							},
						},
						"spec": map[string]interface{}{
							"containers": []map[string]interface{}{
								{
									"name": "filer-sync",
									"command": []string{"/usr/bin/weed", "filer.sync"},
									"args": []string{
										"-a=seaweedfs-filer.seaweedfs.svc.cluster.local:8888",
										"-b=seaweedfs-dr-filer.dr.svc.cluster.local:8888",
										"-a.path=/buckets",
										"-b.path=/backup",
										"-isActivePassive",
										"-metricsPort=9327",
									},
								},
							},
						},
						"status": map[string]interface{}{
							"phase": "Running",
							"podIP": "10.244.2.55",
						},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		http.NotFound(w, r)
	})
}

// -------------------------------------------------------------
// Tests for StaticProvider
// -------------------------------------------------------------

func TestStaticProvider_ListClusters(t *testing.T) {
	clusters := []config.ClusterConfig{
		{
			ID:        "prod",
			Name:      "Production Cluster",
			Namespace: "seaweedfs-prod",
			Masters:   []string{"m1:9333", "m2:9333"},
			Filer:     "filer:8888",
			S3:        "s3:8333",
			FilerSyncs: []config.StaticFilerSyncConfig{
				{
					ID:          "sync-1",
					Name:        "Prod DR Sync",
					MetricsURL:  "http://metrics:9327/metrics",
					SourceFiler: "f1:8888",
					TargetFiler: "f2:8888",
				},
			},
		},
	}
	globalTLS := config.TLSConfig{Enabled: false}

	provider := cluster.NewStaticProvider(clusters, globalTLS)
	list, err := provider.ListClusters(context.Background())
	if err != nil {
		t.Fatalf("ListClusters failed: %v", err)
	}

	if len(list) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(list))
	}
	c := list[0]
	if c.ID != "prod" || c.Name != "Production Cluster" || c.Source != cluster.SourceStatic {
		t.Errorf("cluster attributes mismatch: %+v", c)
	}
	if len(c.Masters) != 2 || c.Filer != "filer:8888" || c.S3 != "s3:8333" {
		t.Errorf("endpoints mismatch: %+v", c)
	}
	if len(c.FilerSyncs) != 1 || c.FilerSyncs[0].ID != "sync-1" {
		t.Errorf("filerSyncs mismatch: %+v", c.FilerSyncs)
	}
}

func TestStaticProvider_SlugifyFallback(t *testing.T) {
	clusters := []config.ClusterConfig{
		{
			ID:      "",
			Name:    "Staging SeaweedFS",
			Masters: []string{"master.stage:9333"},
		},
		{
			ID:      "",
			Name:    "",
			Masters: []string{"master.dev:9333"},
		},
	}
	provider := cluster.NewStaticProvider(clusters, config.TLSConfig{})
	list, err := provider.ListClusters(context.Background())
	if err != nil {
		t.Fatalf("ListClusters failed: %v", err)
	}

	if len(list) != 2 {
		t.Fatalf("expected 2 clusters, got %d", len(list))
	}
	if list[0].ID != "staging-seaweedfs" {
		t.Errorf("expected slugified ID 'staging-seaweedfs', got %q", list[0].ID)
	}
	if !strings.Contains(list[1].ID, "master-dev") {
		t.Errorf("expected slugified ID from master address, got %q", list[1].ID)
	}
}

func TestStaticProvider_TLSInheritance(t *testing.T) {
	globalTLS := config.TLSConfig{
		Enabled: true,
		CAFile:  "/global/ca.crt",
	}
	clusters := []config.ClusterConfig{
		{
			ID:      "inherits-global",
			Masters: []string{"m1:9333"},
		},
		{
			ID:      "overrides-global",
			Masters: []string{"m2:9333"},
			TLS: &config.TLSConfig{
				Enabled:  true,
				CAFile:   "/custom/ca.crt",
				CertFile: "/custom/tls.crt",
				KeyFile:  "/custom/tls.key",
			},
		},
	}

	provider := cluster.NewStaticProvider(clusters, globalTLS)
	list, err := provider.ListClusters(context.Background())
	if err != nil {
		t.Fatalf("ListClusters failed: %v", err)
	}

	if list[0].TLS.CAFile != "/global/ca.crt" {
		t.Errorf("expected inherited CAFile '/global/ca.crt', got %q", list[0].TLS.CAFile)
	}
	if list[1].TLS.CAFile != "/custom/ca.crt" || list[1].TLS.CertFile != "/custom/tls.crt" {
		t.Errorf("expected override CAFile '/custom/ca.crt', got %q", list[1].TLS.CAFile)
	}
}

// -------------------------------------------------------------
// Tests for K8sDiscoveryProvider
// -------------------------------------------------------------

func TestK8sDiscoveryProvider_HeadlessMasterResolution(t *testing.T) {
	client, baseURL := newMockK8sClient(newStandardMockK8sHandler())

	cfg := config.K8sDiscoveryConfig{
		Enabled:     true,
		ClusterWide: true,
	}

	provider := cluster.NewK8sDiscoveryProvider(
		cfg,
		config.TLSConfig{Enabled: false},
		cluster.WithAPIBaseURL(baseURL),
		cluster.WithHTTPClient(client),
		cluster.WithToken("test-sa-token"),
	)

	clusters, err := provider.ListClusters(context.Background())
	if err != nil {
		t.Fatalf("ListClusters failed: %v", err)
	}

	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster discovered, got %d", len(clusters))
	}

	c := clusters[0]
	if c.ID != "k8s-seaweedfs-seaweedfs" {
		t.Errorf("expected cluster ID 'k8s-seaweedfs-seaweedfs', got %q", c.ID)
	}
	if c.Source != cluster.SourceKubernetes {
		t.Errorf("expected SourceKubernetes, got %s", c.Source)
	}

	// Verify headless master resolution to individual pod hostnames
	if len(c.Masters) != 2 {
		t.Fatalf("expected 2 resolved master endpoints, got %d: %v", len(c.Masters), c.Masters)
	}
	if c.Masters[0] != "seaweedfs-master-0.seaweedfs-master.seaweedfs.svc.cluster.local:9333" {
		t.Errorf("expected pod 0 master endpoint, got %q", c.Masters[0])
	}
	if c.Masters[1] != "seaweedfs-master-1.seaweedfs-master.seaweedfs.svc.cluster.local:9333" {
		t.Errorf("expected pod 1 master endpoint, got %q", c.Masters[1])
	}

	// Verify filer and s3 port mapping
	if c.Filer != "seaweedfs-filer.seaweedfs.svc.cluster.local:8888" {
		t.Errorf("expected filer endpoint, got %q", c.Filer)
	}
	if c.S3 != "seaweedfs-s3.seaweedfs.svc.cluster.local:8333" {
		t.Errorf("expected s3 endpoint, got %q", c.S3)
	}
}

func TestK8sDiscoveryProvider_FilerSyncDiscovery(t *testing.T) {
	client, baseURL := newMockK8sClient(newStandardMockK8sHandler())

	cfg := config.K8sDiscoveryConfig{
		Enabled:     true,
		ClusterWide: true,
	}

	provider := cluster.NewK8sDiscoveryProvider(
		cfg,
		config.TLSConfig{Enabled: false},
		cluster.WithAPIBaseURL(baseURL),
		cluster.WithHTTPClient(client),
	)

	clusters, err := provider.ListClusters(context.Background())
	if err != nil {
		t.Fatalf("ListClusters failed: %v", err)
	}

	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster discovered, got %d", len(clusters))
	}

	c := clusters[0]
	if len(c.FilerSyncs) != 1 {
		t.Fatalf("expected 1 filerSync target discovered, got %d", len(c.FilerSyncs))
	}

	sync := c.FilerSyncs[0]
	if sync.SourceFiler != "seaweedfs-filer.seaweedfs.svc.cluster.local:8888" {
		t.Errorf("got SourceFiler %q", sync.SourceFiler)
	}
	if sync.TargetFiler != "seaweedfs-dr-filer.dr.svc.cluster.local:8888" {
		t.Errorf("got TargetFiler %q", sync.TargetFiler)
	}
	if sync.SourcePath != "/buckets" {
		t.Errorf("got SourcePath %q, want /buckets", sync.SourcePath)
	}
	if sync.TargetPath != "/backup" {
		t.Errorf("got TargetPath %q, want /backup", sync.TargetPath)
	}
	if !sync.IsActivePassive {
		t.Errorf("expected IsActivePassive true, got false")
	}
	if sync.MetricsPort != 9327 {
		t.Errorf("got MetricsPort %d, want 9327", sync.MetricsPort)
	}
	if sync.MetricsURL != "http://10.244.2.55:9327/metrics" {
		t.Errorf("got MetricsURL %q", sync.MetricsURL)
	}
}

func TestK8sDiscoveryProvider_GracefulFallback_NonK8s(t *testing.T) {
	cfg := config.K8sDiscoveryConfig{Enabled: true}
	// No options passed -> defaults to non-K8s in test environment
	provider := cluster.NewK8sDiscoveryProvider(cfg, config.TLSConfig{})

	clusters, err := provider.ListClusters(context.Background())
	if err != nil {
		t.Fatalf("expected nil error on non-K8s fallback, got: %v", err)
	}
	if len(clusters) != 0 {
		t.Fatalf("expected 0 clusters in non-K8s fallback, got: %d", len(clusters))
	}
}

func TestK8sDiscoveryProvider_RBACFallback(t *testing.T) {
	// Handler that returns 403 on cluster-wide query and 200 on namespace-scoped query
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/services" {
			http.Error(w, `{"kind":"Status","status":"Failure","message":"forbidden"}`, http.StatusForbidden)
			return
		}
		if r.URL.Path == "/api/v1/namespaces/seaweedfs-local/services" {
			resp := map[string]interface{}{
				"items": []map[string]interface{}{
					{
						"metadata": map[string]interface{}{
							"name":      "seaweedfs-master",
							"namespace": "seaweedfs-local",
							"labels": map[string]string{
								"app.kubernetes.io/name":     "seaweedfs",
								"app.kubernetes.io/instance": "seaweedfs-local",
							},
						},
						"spec": map[string]interface{}{
							"clusterIP": "10.96.0.1",
							"ports": []map[string]interface{}{
								{"name": "master", "port": 9333},
							},
						},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		http.NotFound(w, r)
	})

	client, baseURL := newMockK8sClient(handler)
	cfg := config.K8sDiscoveryConfig{Enabled: true}

	provider := cluster.NewK8sDiscoveryProvider(
		cfg,
		config.TLSConfig{},
		cluster.WithAPIBaseURL(baseURL),
		cluster.WithHTTPClient(client),
		cluster.WithInClusterNamespace("seaweedfs-local"),
	)

	clusters, err := provider.ListClusters(context.Background())
	if err != nil {
		t.Fatalf("expected successful fallback to in-cluster namespace, got: %v", err)
	}
	if len(clusters) != 1 || clusters[0].Namespace != "seaweedfs-local" {
		t.Fatalf("expected 1 cluster in seaweedfs-local, got %+v", clusters)
	}
}

// -------------------------------------------------------------
// Tests for CompositeRegistry
// -------------------------------------------------------------

func TestCompositeRegistry_Collision_StaticPrecedence(t *testing.T) {
	client, baseURL := newMockK8sClient(newStandardMockK8sHandler())

	cfg := &config.Config{
		Discovery: config.DiscoveryConfig{
			Kubernetes: config.K8sDiscoveryConfig{
				Enabled:     true,
				ClusterWide: true,
			},
		},
		Clusters: []config.ClusterConfig{
			{
				ID:        "k8s-seaweedfs-seaweedfs", // matches discovered ID
				Name:      "Production SeaweedFS",
				Namespace: "custom-ns",
				Masters:   []string{"custom-master:9333"},
				TLS: &config.TLSConfig{
					Enabled: true,
					CAFile:  "/custom/ca.crt",
				},
			},
		},
	}

	registry := cluster.NewCompositeRegistry(
		cfg,
		cluster.WithAPIBaseURL(baseURL),
		cluster.WithHTTPClient(client),
	)

	if err := registry.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh failed: %v", err)
	}

	clusters, err := registry.ListClusters(context.Background())
	if err != nil {
		t.Fatalf("ListClusters failed: %v", err)
	}

	if len(clusters) != 1 {
		t.Fatalf("expected exactly 1 cluster after collision merge, got %d", len(clusters))
	}

	c := clusters[0]
	// Static fields take precedence
	if c.Name != "Production SeaweedFS" {
		t.Errorf("expected static Name 'Production SeaweedFS', got %q", c.Name)
	}
	if c.Namespace != "custom-ns" {
		t.Errorf("expected static Namespace 'custom-ns', got %q", c.Namespace)
	}
	if !c.TLSEnabled || c.TLS.CAFile != "/custom/ca.crt" {
		t.Errorf("expected static TLS config preserved, got %+v", c.TLS)
	}
	if c.Source != cluster.SourceStatic {
		t.Errorf("expected SourceStatic for merged collision, got %s", c.Source)
	}
}

func TestCompositeRegistry_Collision_EndpointEnrichment(t *testing.T) {
	client, baseURL := newMockK8sClient(newStandardMockK8sHandler())

	// Static cluster specifies master address that matches discovered headless master
	cfg := &config.Config{
		Discovery: config.DiscoveryConfig{
			Kubernetes: config.K8sDiscoveryConfig{
				Enabled:     true,
				ClusterWide: true,
			},
		},
		Clusters: []config.ClusterConfig{
			{
				ID:      "static-cluster-1",
				Name:    "Static Clustered SeaweedFS",
				Masters: []string{"seaweedfs-master-0.seaweedfs-master.seaweedfs.svc.cluster.local:9333"},
				// Filer and S3 intentionally omitted
			},
		},
	}

	registry := cluster.NewCompositeRegistry(
		cfg,
		cluster.WithAPIBaseURL(baseURL),
		cluster.WithHTTPClient(client),
	)

	if err := registry.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh failed: %v", err)
	}

	clusters, err := registry.ListClusters(context.Background())
	if err != nil {
		t.Fatalf("ListClusters failed: %v", err)
	}

	if len(clusters) != 1 {
		t.Fatalf("expected 1 merged cluster, got %d", len(clusters))
	}

	c := clusters[0]
	if c.ID != "static-cluster-1" {
		t.Errorf("expected static ID 'static-cluster-1', got %q", c.ID)
	}
	// Missing endpoints should be enriched from K8s discovery
	if c.Filer != "seaweedfs-filer.seaweedfs.svc.cluster.local:8888" {
		t.Errorf("expected enriched Filer endpoint, got %q", c.Filer)
	}
	if c.S3 != "seaweedfs-s3.seaweedfs.svc.cluster.local:8333" {
		t.Errorf("expected enriched S3 endpoint, got %q", c.S3)
	}
	if len(c.FilerSyncs) != 1 {
		t.Errorf("expected enriched FilerSyncs count 1, got %d", len(c.FilerSyncs))
	}
}

func TestCompositeRegistry_GetCluster(t *testing.T) {
	cfg := &config.Config{
		Clusters: []config.ClusterConfig{
			{ID: "c1", Name: "Cluster 1", Masters: []string{"m1:9333"}},
			{ID: "c2", Name: "Cluster 2", Masters: []string{"m2:9333"}},
		},
	}
	registry := cluster.NewCompositeRegistry(cfg)
	if err := registry.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh failed: %v", err)
	}

	c, err := registry.GetCluster(context.Background(), "c1")
	if err != nil {
		t.Fatalf("GetCluster('c1') failed: %v", err)
	}
	if c.Name != "Cluster 1" {
		t.Errorf("got cluster Name %q, want 'Cluster 1'", c.Name)
	}

	_, err = registry.GetCluster(context.Background(), "non-existent")
	if err == nil || !errors.Is(err, cluster.ErrClusterNotFound) {
		t.Fatalf("expected ErrClusterNotFound, got: %v", err)
	}
}

func TestCompositeRegistry_ConcurrencyAndThreadSafety(t *testing.T) {
	client, baseURL := newMockK8sClient(newStandardMockK8sHandler())

	cfg := &config.Config{
		Discovery: config.DiscoveryConfig{
			Kubernetes: config.K8sDiscoveryConfig{
				Enabled:     true,
				ClusterWide: true,
			},
		},
		Clusters: []config.ClusterConfig{
			{ID: "static-1", Masters: []string{"m1:9333"}},
		},
	}

	registry := cluster.NewCompositeRegistry(
		cfg,
		cluster.WithAPIBaseURL(baseURL),
		cluster.WithHTTPClient(client),
	)

	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// 10 concurrent readers calling ListClusters
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					_, _ = registry.ListClusters(ctx)
					_, _ = registry.GetCluster(ctx, "static-1")
				}
			}
		}()
	}

	// 2 concurrent refreshers calling Refresh
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					_ = registry.Refresh(ctx)
					time.Sleep(5 * time.Millisecond)
				}
			}
		}()
	}

	wg.Wait()
}

func TestCompositeRegistry_BackgroundRefreshLifecycle(t *testing.T) {
	cfg := &config.Config{
		Discovery: config.DiscoveryConfig{
			Kubernetes: config.K8sDiscoveryConfig{
				Enabled:         false,
				RefreshInterval: 50 * time.Millisecond,
			},
		},
		Clusters: []config.ClusterConfig{
			{ID: "c1", Masters: []string{"m1:9333"}},
		},
	}

	registry := cluster.NewCompositeRegistry(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := registry.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Wait 60ms to let ticker tick
	time.Sleep(60 * time.Millisecond)

	clusters, err := registry.ListClusters(ctx)
	if err != nil || len(clusters) != 1 {
		t.Fatalf("expected 1 cluster after start, got %d (%v)", len(clusters), err)
	}

	registry.Stop()
}
