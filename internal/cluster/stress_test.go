package cluster_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"seaweedfs-ui/internal/cluster"
	"seaweedfs-ui/internal/config"
)

// ---------------------------------------------------------------------------
// 1. HIGH CONCURRENCY & RACE STRESS HARNESS
// ---------------------------------------------------------------------------

func TestAdversarial_HighConcurrencyRaceStress(t *testing.T) {
	var state atomic.Int32

	// Flapping mock handler that alternates response payloads across requests
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		s := state.Add(1) % 4

		if strings.Contains(r.URL.Path, "/services") {
			var items []map[string]interface{}
			switch s {
			case 0:
				// 2 services
				items = append(items,
					map[string]interface{}{
						"metadata": map[string]interface{}{
							"name":      "swfs-master",
							"namespace": "seaweedfs",
							"labels":    map[string]string{"app.kubernetes.io/instance": "sw1"},
						},
						"spec": map[string]interface{}{
							"clusterIP": "10.0.0.1",
							"ports":     []map[string]interface{}{{"name": "swfs-master", "port": 9333}},
						},
					},
					map[string]interface{}{
						"metadata": map[string]interface{}{
							"name":      "swfs-filer",
							"namespace": "seaweedfs",
							"labels":    map[string]string{"app.kubernetes.io/instance": "sw1"},
						},
						"spec": map[string]interface{}{
							"clusterIP": "10.0.0.2",
							"ports":     []map[string]interface{}{{"name": "swfs-filer", "port": 8888}},
						},
					},
				)
			case 1:
				// 3 services (different instance)
				items = append(items,
					map[string]interface{}{
						"metadata": map[string]interface{}{
							"name":      "swfs-master-b",
							"namespace": "seaweedfs-b",
							"labels":    map[string]string{"app.kubernetes.io/instance": "sw2"},
						},
						"spec": map[string]interface{}{
							"clusterIP": "10.0.1.1",
							"ports":     []map[string]interface{}{{"name": "swfs-master", "port": 9333}},
						},
					},
				)
			case 2:
				// Empty services
				items = []map[string]interface{}{}
			case 3:
				// Colliding service matching static-1 master address
				items = append(items,
					map[string]interface{}{
						"metadata": map[string]interface{}{
							"name":      "swfs-master-colliding",
							"namespace": "seaweedfs",
							"labels":    map[string]string{"app.kubernetes.io/instance": "sw-colliding"},
						},
						"spec": map[string]interface{}{
							"clusterIP": "10.99.99.1",
							"ports":     []map[string]interface{}{{"name": "swfs-master", "port": 9333}},
						},
					},
				)
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"items": items})
			return
		}

		if strings.Contains(r.URL.Path, "/pods") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"items": []interface{}{}})
			return
		}

		http.NotFound(w, r)
	})

	client, baseURL := newMockK8sClient(handler)

	cfg := &config.Config{
		Discovery: config.DiscoveryConfig{
			Kubernetes: config.K8sDiscoveryConfig{
				Enabled:     true,
				ClusterWide: true,
			},
		},
		Clusters: []config.ClusterConfig{
			{ID: "static-1", Name: "Static One", Masters: []string{"swfs-master-colliding.seaweedfs.svc.cluster.local:9333"}},
			{ID: "static-2", Name: "Static Two", Masters: []string{"10.200.0.1:9333"}},
		},
	}

	registry := cluster.NewCompositeRegistry(
		cfg,
		cluster.WithAPIBaseURL(baseURL),
		cluster.WithHTTPClient(client),
	)

	// Perform initial refresh
	if err := registry.Refresh(context.Background()); err != nil {
		t.Fatalf("initial refresh failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup

	// 50 concurrent reader goroutines calling ListClusters
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					list, err := registry.ListClusters(ctx)
					if err != nil {
						t.Errorf("ListClusters returned error: %v", err)
						return
					}
					// Verify static clusters always present in list
					if len(list) < 2 {
						t.Errorf("ListClusters returned %d clusters, expected >= 2 static", len(list))
						return
					}
				}
			}
		}()
	}

	// 50 concurrent reader goroutines calling GetCluster
	targetIDs := []string{"static-1", "static-2", "k8s-seaweedfs-sw1", "k8s-seaweedfs-b-sw2", "non-existent"}
	for i := 0; i < 50; i++ {
		wg.Add(1)
		targetID := targetIDs[i%len(targetIDs)]
		go func(id string) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					c, err := registry.GetCluster(ctx, id)
					if id == "static-1" || id == "static-2" {
						if err != nil {
							t.Errorf("GetCluster(%q) failed: %v", id, err)
							return
						}
						if c.ID != id {
							t.Errorf("GetCluster returned ID %q, want %q", c.ID, id)
							return
						}
					}
				}
			}
		}(targetID)
	}

	// 10 concurrent refresher goroutines calling Refresh
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					if err := registry.Refresh(ctx); err != nil {
						t.Errorf("Refresh failed under race: %v", err)
						return
					}
					time.Sleep(2 * time.Millisecond)
				}
			}
		}()
	}

	wg.Wait()
}

// ---------------------------------------------------------------------------
// 2. COLLISION RESOLUTION DEEP MATRIX
// ---------------------------------------------------------------------------

func TestAdversarial_CollisionResolution_IDMatch(t *testing.T) {
	// Discovered cluster matches static cluster by explicit ID
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/services") {
			resp := map[string]interface{}{
				"items": []map[string]interface{}{
					{
						"metadata": map[string]interface{}{
							"name":      "k8s-master",
							"namespace": "seaweedfs",
							// This produces ID: "k8s-seaweedfs-cluster"
							"labels": map[string]string{"app.kubernetes.io/instance": "cluster"},
						},
						"spec": map[string]interface{}{
							"clusterIP": "10.0.0.1",
							"ports":     []map[string]interface{}{{"name": "swfs-master", "port": 9333}},
						},
					},
					{
						"metadata": map[string]interface{}{
							"name":      "k8s-filer",
							"namespace": "seaweedfs",
							"labels":    map[string]string{"app.kubernetes.io/instance": "cluster"},
						},
						"spec": map[string]interface{}{
							"clusterIP": "10.0.0.2",
							"ports":     []map[string]interface{}{{"name": "swfs-filer", "port": 8888}},
						},
					},
					{
						"metadata": map[string]interface{}{
							"name":      "k8s-s3",
							"namespace": "seaweedfs",
							"labels":    map[string]string{"app.kubernetes.io/instance": "cluster"},
						},
						"spec": map[string]interface{}{
							"clusterIP": "10.0.0.3",
							"ports":     []map[string]interface{}{{"name": "swfs-s3", "port": 8333}},
						},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		if strings.Contains(r.URL.Path, "/pods") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"items": []interface{}{}})
			return
		}
		http.NotFound(w, r)
	})

	client, baseURL := newMockK8sClient(handler)

	// Static cluster shares EXACT ID with discovered cluster
	cfg := &config.Config{
		Discovery: config.DiscoveryConfig{
			Kubernetes: config.K8sDiscoveryConfig{
				Enabled:     true,
				ClusterWide: true,
			},
		},
		Clusters: []config.ClusterConfig{
			{
				ID:        "k8s-seaweedfs-cluster",
				Name:      "Authoritative Static Name",
				Namespace: "authoritative-namespace",
				Masters:   []string{"static-master.corp.internal:9333"},
				// Filer and S3 intentionally empty to verify enrichment
				TLS: &config.TLSConfig{
					Enabled:  true,
					CAFile:   "/etc/certs/static-ca.crt",
					CertFile: "/etc/certs/static.crt",
					KeyFile:  "/etc/certs/static.key",
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
		t.Fatalf("expected exactly 1 merged cluster, got %d", len(clusters))
	}

	c := clusters[0]

	// 1. Static fields preserved
	if c.ID != "k8s-seaweedfs-cluster" {
		t.Errorf("expected ID 'k8s-seaweedfs-cluster', got %q", c.ID)
	}
	if c.Name != "Authoritative Static Name" {
		t.Errorf("expected Name 'Authoritative Static Name', got %q", c.Name)
	}
	if c.Namespace != "authoritative-namespace" {
		t.Errorf("expected Namespace 'authoritative-namespace', got %q", c.Namespace)
	}
	if c.Source != cluster.SourceStatic {
		t.Errorf("expected SourceStatic, got %v", c.Source)
	}
	if len(c.Masters) != 1 || c.Masters[0] != "static-master.corp.internal:9333" {
		t.Errorf("expected static Masters preserved, got %+v", c.Masters)
	}
	if !c.TLSEnabled || c.TLS.CAFile != "/etc/certs/static-ca.crt" {
		t.Errorf("expected static TLS preserved, got %+v", c.TLS)
	}

	// 2. Empty fields enriched from K8s discovery
	expectedFiler := "k8s-filer.seaweedfs.svc.cluster.local:8888"
	if c.Filer != expectedFiler {
		t.Errorf("expected enriched Filer %q, got %q", expectedFiler, c.Filer)
	}
	expectedS3 := "k8s-s3.seaweedfs.svc.cluster.local:8333"
	if c.S3 != expectedS3 {
		t.Errorf("expected enriched S3 %q, got %q", expectedS3, c.S3)
	}
}

func TestAdversarial_CollisionResolution_MasterMatch_PreserveNonEmptyFields(t *testing.T) {
	// Static cluster has non-empty Filer and S3.
	// Discovered cluster matches on Master endpoint collision.
	// Verify static non-empty Filer and S3 are NOT overwritten!
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/services") {
			resp := map[string]interface{}{
				"items": []map[string]interface{}{
					{
						"metadata": map[string]interface{}{
							"name":      "seaweedfs-master",
							"namespace": "production",
							"labels":    map[string]string{"app.kubernetes.io/instance": "swfs"},
						},
						"spec": map[string]interface{}{
							"clusterIP": "10.0.0.1",
							"ports":     []map[string]interface{}{{"name": "swfs-master", "port": 9333}},
						},
					},
					{
						"metadata": map[string]interface{}{
							"name":      "seaweedfs-filer",
							"namespace": "production",
							"labels":    map[string]string{"app.kubernetes.io/instance": "swfs"},
						},
						"spec": map[string]interface{}{
							"clusterIP": "10.0.0.2",
							"ports":     []map[string]interface{}{{"name": "swfs-filer", "port": 8888}},
						},
					},
					{
						"metadata": map[string]interface{}{
							"name":      "seaweedfs-s3",
							"namespace": "production",
							"labels":    map[string]string{"app.kubernetes.io/instance": "swfs"},
						},
						"spec": map[string]interface{}{
							"clusterIP": "10.0.0.3",
							"ports":     []map[string]interface{}{{"name": "swfs-s3", "port": 8333}},
						},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		if strings.Contains(r.URL.Path, "/pods") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"items": []interface{}{}})
			return
		}
		http.NotFound(w, r)
	})

	client, baseURL := newMockK8sClient(handler)

	cfg := &config.Config{
		Discovery: config.DiscoveryConfig{
			Kubernetes: config.K8sDiscoveryConfig{
				Enabled:     true,
				ClusterWide: true,
			},
		},
		Clusters: []config.ClusterConfig{
			{
				ID:        "static-override-id",
				Name:      "Static Override Cluster",
				Namespace: "static-ns",
				// Matches master with uppercase/scheme differences
				Masters: []string{"HTTP://SEAWEEDFS-MASTER.production.svc.cluster.local:9333"},
				Filer:   "custom-static-filer:8888",
				S3:      "custom-static-s3:8333",
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
	if c.ID != "static-override-id" {
		t.Errorf("expected static ID 'static-override-id', got %q", c.ID)
	}
	// Static non-empty endpoints must NOT be overwritten by K8s
	if c.Filer != "custom-static-filer:8888" {
		t.Errorf("static Filer was overwritten: got %q, want 'custom-static-filer:8888'", c.Filer)
	}
	if c.S3 != "custom-static-s3:8333" {
		t.Errorf("static S3 was overwritten: got %q, want 'custom-static-s3:8333'", c.S3)
	}
	if c.Namespace != "static-ns" {
		t.Errorf("static Namespace was overwritten: got %q, want 'static-ns'", c.Namespace)
	}

	// Discovered ID must NOT exist as a standalone cluster
	_, err = registry.GetCluster(context.Background(), "k8s-production-swfs")
	if err == nil {
		t.Errorf("expected discovered ID to be subsumed by static cluster, but it was found")
	}
}

func TestAdversarial_CollisionResolution_FilerSyncDeduplication(t *testing.T) {
	// Discovered cluster matches static cluster and provides FilerSyncs.
	// One FilerSync is a duplicate of the static sync, one is distinct.
	// Verify duplicate is filtered while distinct is enriched.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/services") {
			resp := map[string]interface{}{
				"items": []map[string]interface{}{
					{
						"metadata": map[string]interface{}{
							"name":      "swfs-master",
							"namespace": "seaweedfs",
							"labels":    map[string]string{"app.kubernetes.io/instance": "cluster"},
						},
						"spec": map[string]interface{}{
							"clusterIP": "10.0.0.1",
							"ports":     []map[string]interface{}{{"name": "swfs-master", "port": 9333}},
						},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		if strings.Contains(r.URL.Path, "/pods") {
			resp := map[string]interface{}{
				"items": []map[string]interface{}{
					{
						// Duplicate sync (same source/target filer, same paths)
						"metadata": map[string]interface{}{
							"name":      "filer-sync-duplicate",
							"namespace": "seaweedfs",
							"labels":    map[string]string{"app.kubernetes.io/component": "filer-sync"},
						},
						"spec": map[string]interface{}{
							"containers": []map[string]interface{}{
								{
									"name": "filer-sync",
									"command": []string{"/usr/bin/weed", "filer.sync"},
									"args": []string{
										"-a", "filer1:8888",
										"-b", "filer2:8888",
										"-a.path", "/",
										"-b.path", "/",
									},
								},
							},
						},
						"status": map[string]interface{}{
							"podIP": "10.244.0.10",
						},
					},
					{
						// Distinct new sync
						"metadata": map[string]interface{}{
							"name":      "filer-sync-distinct",
							"namespace": "seaweedfs",
							"labels":    map[string]string{"app.kubernetes.io/component": "filer-sync"},
						},
						"spec": map[string]interface{}{
							"containers": []map[string]interface{}{
								{
									"name": "filer-sync",
									"command": []string{"/usr/bin/weed", "filer.sync"},
									"args": []string{
										"-a", "filer1:8888",
										"-b", "filer3:8888",
										"-a.path", "/logs",
										"-b.path", "/backup/logs",
										"-metricsPort=9327",
									},
								},
							},
						},
						"status": map[string]interface{}{
							"podIP": "10.244.0.11",
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

	cfg := &config.Config{
		Discovery: config.DiscoveryConfig{
			Kubernetes: config.K8sDiscoveryConfig{
				Enabled:     true,
				ClusterWide: true,
			},
		},
		Clusters: []config.ClusterConfig{
			{
				ID:      "k8s-seaweedfs-cluster",
				Masters: []string{"swfs-master.seaweedfs.svc.cluster.local:9333"},
				FilerSyncs: []config.StaticFilerSyncConfig{
					{
						ID:          "static-sync-1",
						Name:        "Static Sync 1",
						SourceFiler: "filer1:8888",
						TargetFiler: "filer2:8888",
					},
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

	c, err := registry.GetCluster(context.Background(), "k8s-seaweedfs-cluster")
	if err != nil {
		t.Fatalf("GetCluster failed: %v", err)
	}

	// Should have exactly 2 FilerSync targets (original static + 1 distinct discovered)
	if len(c.FilerSyncs) != 2 {
		t.Fatalf("expected 2 FilerSyncs (deduped duplicate), got %d: %+v", len(c.FilerSyncs), c.FilerSyncs)
	}

	if c.FilerSyncs[0].ID != "static-sync-1" {
		t.Errorf("expected first sync to be static 'static-sync-1', got %q", c.FilerSyncs[0].ID)
	}
	if c.FilerSyncs[1].TargetFiler != "filer3:8888" {
		t.Errorf("expected second sync target filer 'filer3:8888', got %q", c.FilerSyncs[1].TargetFiler)
	}
}

// ---------------------------------------------------------------------------
// 3. NON-K8S DEGRADATION HARNESS
// ---------------------------------------------------------------------------

func TestAdversarial_NonK8sDegradation(t *testing.T) {
	// Explicitly clear any Kubernetes environment variables
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	// Construct configuration with K8s discovery enabled
	cfg := &config.Config{
		Discovery: config.DiscoveryConfig{
			Kubernetes: config.K8sDiscoveryConfig{
				Enabled:     true,
				ClusterWide: true,
			},
		},
		Clusters: []config.ClusterConfig{
			{
				ID:      "workstation-local",
				Name:    "Workstation Local Cluster",
				Masters: []string{"127.0.0.1:9333"},
			},
		},
	}

	// Create registry without WithAPIBaseURL so it executes initInClusterConfig()
	registry := cluster.NewCompositeRegistry(cfg)

	// Refresh MUST succeed without error, crash, or panic
	if err := registry.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh failed in non-K8s environment: %v", err)
	}

	clusters, err := registry.ListClusters(context.Background())
	if err != nil {
		t.Fatalf("ListClusters failed: %v", err)
	}

	if len(clusters) != 1 {
		t.Fatalf("expected exactly 1 static cluster in non-K8s mode, got %d", len(clusters))
	}

	if clusters[0].ID != "workstation-local" {
		t.Errorf("expected cluster 'workstation-local', got %q", clusters[0].ID)
	}

	c, err := registry.GetCluster(context.Background(), "workstation-local")
	if err != nil {
		t.Fatalf("GetCluster failed: %v", err)
	}
	if c.Source != cluster.SourceStatic {
		t.Errorf("expected SourceStatic, got %v", c.Source)
	}

	// Direct instantiation of K8sDiscoveryProvider
	k8sProv := cluster.NewK8sDiscoveryProvider(cfg.Discovery.Kubernetes, cfg.TLS)
	discovered, err := k8sProv.ListClusters(context.Background())
	if err != nil {
		t.Fatalf("k8sProv.ListClusters returned unexpected error: %v", err)
	}
	if len(discovered) != 0 {
		t.Fatalf("expected 0 discovered clusters in non-K8s mode, got %d", len(discovered))
	}
}

// ---------------------------------------------------------------------------
// 4. MALFORMED & FLAKY K8S API RESPONSES
// ---------------------------------------------------------------------------

func TestAdversarial_FlakyK8sAPI_HTTP500_Services(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"kind":"Status","status":"Failure","message":"internal server error"}`))
	})

	client, baseURL := newMockK8sClient(handler)

	cfg := &config.Config{
		Discovery: config.DiscoveryConfig{
			Kubernetes: config.K8sDiscoveryConfig{Enabled: true},
		},
		Clusters: []config.ClusterConfig{
			{ID: "survivor-1", Masters: []string{"10.0.0.1:9333"}},
		},
	}

	registry := cluster.NewCompositeRegistry(
		cfg,
		cluster.WithAPIBaseURL(baseURL),
		cluster.WithHTTPClient(client),
	)

	// Refresh must not fail; it should log warning and return static clusters
	if err := registry.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh should not return error when K8s 500s: %v", err)
	}

	clusters, err := registry.ListClusters(context.Background())
	if err != nil {
		t.Fatalf("ListClusters failed: %v", err)
	}

	if len(clusters) != 1 || clusters[0].ID != "survivor-1" {
		t.Fatalf("expected static cluster to survive K8s HTTP 500, got: %+v", clusters)
	}
}

func TestAdversarial_FlakyK8sAPI_HTTP500_Pods(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/services") {
			resp := map[string]interface{}{
				"items": []map[string]interface{}{
					{
						"metadata": map[string]interface{}{
							"name":      "swfs-master",
							"namespace": "seaweedfs",
							"labels":    map[string]string{"app.kubernetes.io/instance": "sw1"},
						},
						"spec": map[string]interface{}{
							"clusterIP": "10.0.0.1",
							"ports":     []map[string]interface{}{{"name": "swfs-master", "port": 9333}},
						},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		if strings.Contains(r.URL.Path, "/pods") {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"status":"Failure","message":"etcd pod listing timed out"}`))
			return
		}

		http.NotFound(w, r)
	})

	client, baseURL := newMockK8sClient(handler)

	cfg := &config.Config{
		Discovery: config.DiscoveryConfig{
			Kubernetes: config.K8sDiscoveryConfig{Enabled: true},
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
		t.Fatalf("expected 1 cluster discovered despite pods 500 error, got %d", len(clusters))
	}

	if len(clusters[0].FilerSyncs) != 0 {
		t.Errorf("expected 0 FilerSyncs when pods fail, got %d", len(clusters[0].FilerSyncs))
	}
}

func TestAdversarial_FlakyK8sAPI_CorruptedJSON(t *testing.T) {
	testCases := []struct {
		name       string
		brokenPath string
	}{
		{"corrupted services JSON", "/services"},
		{"corrupted pods JSON", "/pods"},
		{"corrupted endpoints JSON", "/endpoints"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(r.URL.Path, tc.brokenPath) {
					w.WriteHeader(http.StatusOK)
					// Truncated, unclosed JSON
					_, _ = w.Write([]byte(`{"items": [{"metadata": {"name": "swfs"`))
					return
				}
				// Default valid responses for other paths
				if strings.Contains(r.URL.Path, "/services") {
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"items": []interface{}{}})
					return
				}
				if strings.Contains(r.URL.Path, "/pods") {
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"items": []interface{}{}})
					return
				}
				http.NotFound(w, r)
			})

			client, baseURL := newMockK8sClient(handler)

			cfg := &config.Config{
				Discovery: config.DiscoveryConfig{
					Kubernetes: config.K8sDiscoveryConfig{Enabled: true},
				},
				Clusters: []config.ClusterConfig{
					{ID: "stable-static", Masters: []string{"10.0.0.1:9333"}},
				},
			}

			registry := cluster.NewCompositeRegistry(
				cfg,
				cluster.WithAPIBaseURL(baseURL),
				cluster.WithHTTPClient(client),
			)

			if err := registry.Refresh(context.Background()); err != nil {
				t.Fatalf("Refresh must not fail when JSON is corrupted: %v", err)
			}

			clusters, err := registry.ListClusters(context.Background())
			if err != nil {
				t.Fatalf("ListClusters failed: %v", err)
			}

			if len(clusters) < 1 || clusters[0].ID != "stable-static" {
				t.Errorf("static cluster must survive corrupted JSON, got: %+v", clusters)
			}
		})
	}
}

func TestAdversarial_FlakyK8sAPI_HeadlessZeroEndpoints(t *testing.T) {
	testVariations := []struct {
		name     string
		subsets  []map[string]interface{}
		httpCode int
	}{
		{
			name:     "404 endpoints not found",
			httpCode: http.StatusNotFound,
		},
		{
			name:     "500 endpoints internal error",
			httpCode: http.StatusInternalServerError,
		},
		{
			name:     "empty subsets array",
			httpCode: http.StatusOK,
			subsets:  []map[string]interface{}{},
		},
		{
			name:     "subset with empty addresses array",
			httpCode: http.StatusOK,
			subsets: []map[string]interface{}{
				{"addresses": []interface{}{}},
			},
		},
		{
			name:     "subset with completely empty address struct",
			httpCode: http.StatusOK,
			subsets: []map[string]interface{}{
				{
					"addresses": []map[string]interface{}{
						{"ip": "", "hostname": "", "targetRef": nil},
					},
				},
			},
		},
	}

	for _, tv := range testVariations {
		t.Run(tv.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")

				if strings.Contains(r.URL.Path, "/services") {
					resp := map[string]interface{}{
						"items": []map[string]interface{}{
							{
								"metadata": map[string]interface{}{
									"name":      "swfs-master-headless",
									"namespace": "seaweedfs",
									"labels":    map[string]string{"app.kubernetes.io/instance": "sw1"},
								},
								"spec": map[string]interface{}{
									"clusterIP": "None", // Headless
									"ports":     []map[string]interface{}{{"name": "swfs-master", "port": 9333}},
								},
							},
						},
					}
					_ = json.NewEncoder(w).Encode(resp)
					return
				}

				if strings.Contains(r.URL.Path, "/endpoints") {
					if tv.httpCode != http.StatusOK {
						w.WriteHeader(tv.httpCode)
						_, _ = w.Write([]byte(`{"status":"Failure"}`))
						return
					}
					resp := map[string]interface{}{
						"metadata": map[string]interface{}{
							"name":      "swfs-master-headless",
							"namespace": "seaweedfs",
						},
						"subsets": tv.subsets,
					}
					_ = json.NewEncoder(w).Encode(resp)
					return
				}

				if strings.Contains(r.URL.Path, "/pods") {
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"items": []interface{}{}})
					return
				}

				http.NotFound(w, r)
			})

			client, baseURL := newMockK8sClient(handler)

			cfg := &config.Config{
				Discovery: config.DiscoveryConfig{
					Kubernetes: config.K8sDiscoveryConfig{Enabled: true},
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
				t.Fatalf("expected 1 cluster discovered via FQDN fallback, got %d", len(clusters))
			}

			c := clusters[0]
			expectedFallbackMaster := "swfs-master-headless.seaweedfs.svc.cluster.local:9333"
			if len(c.Masters) != 1 || c.Masters[0] != expectedFallbackMaster {
				t.Errorf("expected fallback master %q, got %+v", expectedFallbackMaster, c.Masters)
			}
		})
	}
}

func TestAdversarial_FlakyK8sAPI_GatewayTimeoutHTML(t *testing.T) {
	// K8s API proxy returns 504 Gateway Timeout HTML page
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusGatewayTimeout)
		_, _ = w.Write([]byte(`<html><head><title>504 Gateway Time-out</title></head><body><center><h1>504 Gateway Time-out</h1></center><hr><center>nginx/1.25.1</center></body></html>`))
	})

	client, baseURL := newMockK8sClient(handler)

	cfg := &config.Config{
		Discovery: config.DiscoveryConfig{
			Kubernetes: config.K8sDiscoveryConfig{Enabled: true},
		},
		Clusters: []config.ClusterConfig{
			{ID: "static-survivor", Masters: []string{"10.0.0.1:9333"}},
		},
	}

	registry := cluster.NewCompositeRegistry(
		cfg,
		cluster.WithAPIBaseURL(baseURL),
		cluster.WithHTTPClient(client),
	)

	if err := registry.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh failed on HTML 504: %v", err)
	}

	clusters, err := registry.ListClusters(context.Background())
	if err != nil {
		t.Fatalf("ListClusters failed: %v", err)
	}

	if len(clusters) != 1 || clusters[0].ID != "static-survivor" {
		t.Fatalf("expected static cluster to survive 504 HTML error, got %+v", clusters)
	}
}

func TestAdversarial_FlakyK8sAPI_AdversarialFilerSyncArgs(t *testing.T) {
	// Verify that bizarre, truncated, or adversarial container arguments
	// in filer.sync pods never cause index-out-of-range panics.
	adversarialPods := []map[string]interface{}{
		{
			"metadata": map[string]interface{}{"name": "pod-trailing-flag-a", "namespace": "seaweedfs"},
			"spec": map[string]interface{}{
				"containers": []map[string]interface{}{
					{"command": []string{"weed", "filer.sync"}, "args": []string{"-a"}}, // -a at end of slice
				},
			},
		},
		{
			"metadata": map[string]interface{}{"name": "pod-trailing-flag-b", "namespace": "seaweedfs"},
			"spec": map[string]interface{}{
				"containers": []map[string]interface{}{
					{"command": []string{"weed", "filer.sync"}, "args": []string{"-b"}}, // -b at end of slice
				},
			},
		},
		{
			"metadata": map[string]interface{}{"name": "pod-trailing-metric-port", "namespace": "seaweedfs"},
			"spec": map[string]interface{}{
				"containers": []map[string]interface{}{
					{"command": []string{"weed", "filer.sync"}, "args": []string{"-a=f1:8888", "-metricsPort"}}, // -metricsPort at end
				},
			},
		},
		{
			"metadata": map[string]interface{}{"name": "pod-invalid-metric-port", "namespace": "seaweedfs"},
			"spec": map[string]interface{}{
				"containers": []map[string]interface{}{
					{"command": []string{"weed", "filer.sync"}, "args": []string{"-a=f1:8888", "-metricsPort=notanumber"}},
				},
			},
		},
		{
			"metadata": map[string]interface{}{"name": "pod-empty-flags", "namespace": "seaweedfs"},
			"spec": map[string]interface{}{
				"containers": []map[string]interface{}{
					{"command": []string{"weed", "filer.sync"}, "args": []string{"-a=", "-b=", "-a.path=", "-b.path="}},
				},
			},
		},
		{
			"metadata": map[string]interface{}{"name": "pod-empty-command-and-args", "namespace": "seaweedfs"},
			"spec": map[string]interface{}{
				"containers": []map[string]interface{}{
					{"command": []string{}, "args": []string{}},
				},
			},
		},
		{
			"metadata": map[string]interface{}{"name": "pod-massive-tokens", "namespace": "seaweedfs"},
			"spec": map[string]interface{}{
				"containers": []map[string]interface{}{
					{
						"command": []string{"weed", "filer.sync"},
						"args": func() []string {
							toks := make([]string, 1000)
							for i := range toks {
								toks[i] = fmt.Sprintf("--arg-%d=val-%d", i, i)
							}
							return toks
						}(),
					},
				},
			},
		},
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/services") {
			resp := map[string]interface{}{
				"items": []map[string]interface{}{
					{
						"metadata": map[string]interface{}{
							"name":      "swfs-master",
							"namespace": "seaweedfs",
							"labels":    map[string]string{"app.kubernetes.io/instance": "swfs"},
						},
						"spec": map[string]interface{}{
							"clusterIP": "10.0.0.1",
							"ports":     []map[string]interface{}{{"name": "swfs-master", "port": 9333}},
						},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		if strings.Contains(r.URL.Path, "/pods") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"items": adversarialPods})
			return
		}

		http.NotFound(w, r)
	})

	client, baseURL := newMockK8sClient(handler)

	cfg := &config.Config{
		Discovery: config.DiscoveryConfig{
			Kubernetes: config.K8sDiscoveryConfig{Enabled: true},
		},
	}

	registry := cluster.NewCompositeRegistry(
		cfg,
		cluster.WithAPIBaseURL(baseURL),
		cluster.WithHTTPClient(client),
	)

	// Must cleanly handle all edge cases without panic
	if err := registry.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh failed on adversarial filer.sync args: %v", err)
	}

	clusters, err := registry.ListClusters(context.Background())
	if err != nil {
		t.Fatalf("ListClusters failed: %v", err)
	}

	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster discovered, got %d", len(clusters))
	}
}

// ---------------------------------------------------------------------------
// 5. CONTEXT CANCELLATION & IMMEDIATE TIMEOUT
// ---------------------------------------------------------------------------

func TestAdversarial_ContextCancellation(t *testing.T) {
	cfg := &config.Config{
		Clusters: []config.ClusterConfig{
			{ID: "c1", Masters: []string{"10.0.0.1:9333"}},
		},
	}

	registry := cluster.NewCompositeRegistry(cfg)

	// Refresh with active context first
	if err := registry.Refresh(context.Background()); err != nil {
		t.Fatalf("initial refresh failed: %v", err)
	}

	// Now call methods with pre-canceled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	list, err := registry.ListClusters(ctx)
	if err != nil {
		t.Errorf("ListClusters should return in-memory snapshot even if ctx canceled: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("expected 1 cluster, got %d", len(list))
	}

	c, err := registry.GetCluster(ctx, "c1")
	if err != nil {
		t.Errorf("GetCluster should return in-memory cluster even if ctx canceled: %v", err)
	}
	if c.ID != "c1" {
		t.Errorf("expected ID 'c1', got %q", c.ID)
	}
}
