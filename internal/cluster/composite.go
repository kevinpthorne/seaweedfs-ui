package cluster

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"seaweedfs-ui/internal/config"
)

// CompositeRegistry unifies static clusters from configuration and dynamically discovered clusters.
type CompositeRegistry struct {
	mu              sync.RWMutex
	staticProvider  ClusterProvider
	k8sProvider     ClusterProvider
	refreshInterval time.Duration
	clusters        []ClusterTarget
	clusterMap      map[string]ClusterTarget
	stopCh          chan struct{}
	stopOnce        sync.Once
}

// NewCompositeRegistry constructs a new CompositeRegistry.
func NewCompositeRegistry(cfg *config.Config, k8sOpts ...K8sDiscoveryOption) *CompositeRegistry {
	staticProv := NewStaticProvider(cfg.Clusters, cfg.TLS)

	var k8sProv ClusterProvider
	if cfg.Discovery.Kubernetes.Enabled {
		k8sProv = NewK8sDiscoveryProvider(cfg.Discovery.Kubernetes, cfg.TLS, k8sOpts...)
	}

	interval := cfg.Discovery.Kubernetes.RefreshInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}

	return &CompositeRegistry{
		staticProvider:  staticProv,
		k8sProvider:     k8sProv,
		refreshInterval: interval,
		clusterMap:      make(map[string]ClusterTarget),
		stopCh:          make(chan struct{}),
	}
}

// ListClusters returns a snapshot of all registered SeaweedFS clusters.
func (r *CompositeRegistry) ListClusters(ctx context.Context) ([]ClusterTarget, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]ClusterTarget, len(r.clusters))
	copy(result, r.clusters)
	return result, nil
}

// GetCluster returns a specific SeaweedFS cluster by its ID.
func (r *CompositeRegistry) GetCluster(ctx context.Context, id string) (*ClusterTarget, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	target, exists := r.clusterMap[id]
	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrClusterNotFound, id)
	}

	clone := target
	return &clone, nil
}

// Refresh re-queries static and discovered providers and atomically merges their results.
func (r *CompositeRegistry) Refresh(ctx context.Context) error {
	// 1. Gather static clusters
	staticList, err := r.staticProvider.ListClusters(ctx)
	if err != nil {
		return fmt.Errorf("list static clusters: %w", err)
	}

	// 2. Gather discovered clusters
	var k8sList []ClusterTarget
	if r.k8sProvider != nil {
		discovered, err := r.k8sProvider.ListClusters(ctx)
		if err != nil {
			log.Printf("[WARN] K8s discovery refresh error: %v", err)
		} else {
			k8sList = discovered
		}
	}

	// 3. Collision Resolution & Merging
	merged := make(map[string]ClusterTarget)
	masterToID := make(map[string]string)

	for _, s := range staticList {
		merged[s.ID] = s
		for _, m := range s.Masters {
			masterToID[normalizeEndpoint(m)] = s.ID
		}
	}

	for _, k := range k8sList {
		matchedID := ""

		// Check by ID
		if _, exists := merged[k.ID]; exists {
			matchedID = k.ID
		}

		// Check by Master endpoint collision
		if matchedID == "" {
			for _, km := range k.Masters {
				if id, exists := masterToID[normalizeEndpoint(km)]; exists {
					matchedID = id
					break
				}
			}
		}

		if matchedID != "" {
			// Collision! Static takes precedence, enrich missing fields
			existing := merged[matchedID]

			if existing.Filer == "" && k.Filer != "" {
				existing.Filer = k.Filer
			}
			if existing.S3 == "" && k.S3 != "" {
				existing.S3 = k.S3
			}
			if existing.Namespace == "" && k.Namespace != "" {
				existing.Namespace = k.Namespace
			}

			// Enrich FilerSyncs
			for _, sync := range k.FilerSyncs {
				if !containsSync(existing.FilerSyncs, sync) {
					existing.FilerSyncs = append(existing.FilerSyncs, sync)
				}
			}

			merged[matchedID] = existing
		} else {
			// No collision: register discovered cluster
			merged[k.ID] = k
		}
	}

	// 4. Update in-memory cache atomically
	clusters := make([]ClusterTarget, 0, len(merged))
	for _, target := range merged {
		clusters = append(clusters, target)
	}

	sort.Slice(clusters, func(i, j int) bool {
		if clusters[i].Source != clusters[j].Source {
			return clusters[i].Source == SourceStatic // static clusters first
		}
		return clusters[i].ID < clusters[j].ID
	})

	r.mu.Lock()
	r.clusters = clusters
	r.clusterMap = merged
	r.mu.Unlock()

	return nil
}

// Start initiates the initial refresh and periodic background discovery loop.
func (r *CompositeRegistry) Start(ctx context.Context) error {
	if err := r.Refresh(ctx); err != nil {
		log.Printf("[WARN] Initial registry refresh encountered error: %v", err)
	}

	go func() {
		ticker := time.NewTicker(r.refreshInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-r.stopCh:
				return
			case <-ticker.C:
				if err := r.Refresh(ctx); err != nil {
					log.Printf("[WARN] Periodic registry refresh error: %v", err)
				}
			}
		}
	}()

	return nil
}

// Stop halts the periodic discovery background loop.
func (r *CompositeRegistry) Stop() {
	r.stopOnce.Do(func() {
		close(r.stopCh)
	})
}

func normalizeEndpoint(ep string) string {
	ep = strings.ToLower(strings.TrimSpace(ep))
	ep = strings.TrimPrefix(ep, "http://")
	ep = strings.TrimPrefix(ep, "https://")
	return ep
}

func containsSync(list []FilerSyncTarget, s FilerSyncTarget) bool {
	for _, item := range list {
		if item.ID == s.ID {
			return true
		}
		if item.SourceFiler == s.SourceFiler && item.TargetFiler == s.TargetFiler && item.SourcePath == s.SourcePath && item.TargetPath == s.TargetPath {
			return true
		}
	}
	return false
}
