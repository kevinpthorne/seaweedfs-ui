package cluster

import (
	"context"
	"fmt"
	"strings"

	"seaweedfs-ui/internal/config"
)

// StaticProvider loads SeaweedFS clusters defined statically in configuration.
type StaticProvider struct {
	clusters  []config.ClusterConfig
	globalTLS config.TLSConfig
}

// NewStaticProvider constructs a new StaticProvider.
func NewStaticProvider(clusters []config.ClusterConfig, globalTLS config.TLSConfig) *StaticProvider {
	return &StaticProvider{
		clusters:  clusters,
		globalTLS: globalTLS,
	}
}

// ListClusters returns all statically configured SeaweedFS clusters.
func (p *StaticProvider) ListClusters(ctx context.Context) ([]ClusterTarget, error) {
	targets := make([]ClusterTarget, 0, len(p.clusters))

	for i, c := range p.clusters {
		id := strings.TrimSpace(c.ID)
		name := strings.TrimSpace(c.Name)

		if id == "" {
			if name != "" {
				id = slugify(name)
			} else if len(c.Masters) > 0 {
				id = slugify(c.Masters[0])
			} else {
				id = fmt.Sprintf("static-%d", i+1)
			}
		}

		if name == "" {
			name = id
		}

		// TLS resolution: per-cluster overrides global
		tlsCfg := p.globalTLS
		if c.TLS != nil {
			tlsCfg = *c.TLS
		}

		// Convert static filerSyncs
		syncTargets := make([]FilerSyncTarget, 0, len(c.FilerSyncs))
		for si, s := range c.FilerSyncs {
			syncID := s.ID
			if syncID == "" {
				syncID = fmt.Sprintf("%s-sync-%d", id, si+1)
			}
			syncName := s.Name
			if syncName == "" {
				syncName = syncID
			}

			syncTargets = append(syncTargets, FilerSyncTarget{
				ID:              syncID,
				Name:            syncName,
				DiscoverySource: SourceStatic,
				MetricsURL:      s.MetricsURL,
				SourceFiler:     s.SourceFiler,
				TargetFiler:     s.TargetFiler,
				SourcePath:      "/",
				TargetPath:      "/",
				IsActivePassive: false,
			})
		}

		masters := make([]string, len(c.Masters))
		copy(masters, c.Masters)

		targets = append(targets, ClusterTarget{
			ID:         id,
			Name:       name,
			Namespace:  c.Namespace,
			Source:     SourceStatic,
			Masters:    masters,
			Filer:      c.Filer,
			S3:         c.S3,
			TLS:        tlsCfg,
			TLSEnabled: tlsCfg.Enabled,
			FilerSyncs: syncTargets,
			Labels:     map[string]string{"type": "static"},
		})
	}

	return targets, nil
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, ":", "-")
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, ".", "-")
	return s
}
