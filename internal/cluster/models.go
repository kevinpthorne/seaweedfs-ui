package cluster

import (
	"errors"
	"time"

	"seaweedfs-ui/internal/config"
)

var (
	// ErrClusterNotFound is returned when a requested cluster ID does not exist in the registry.
	ErrClusterNotFound = errors.New("cluster not found")
)

// ClusterSource identifies how the cluster was registered (static configuration vs. K8s auto-discovery).
type ClusterSource string

const (
	SourceStatic     ClusterSource = "static"
	SourceKubernetes ClusterSource = "kubernetes"
)

// ClusterHealthState represents the Raft/Master reachability status.
type ClusterHealthState string

const (
	HealthHealthy     ClusterHealthState = "healthy"
	HealthDegraded    ClusterHealthState = "degraded"
	HealthUnreachable ClusterHealthState = "unreachable"
)

// ClusterHealth details active master leadership and reachability.
type ClusterHealth struct {
	State       ClusterHealthState `json:"state"`
	Leader      string             `json:"leader"`
	IsLeader    bool               `json:"isLeader"`
	ActivePeers int                `json:"activePeers"`
	TotalPeers  int                `json:"totalPeers"`
	Message     string             `json:"message,omitempty"`
	LastChecked time.Time          `json:"lastChecked"`
}

// FilerSyncTarget represents a running weed filer.sync replication pair.
type FilerSyncTarget struct {
	ID              string            `json:"id"`
	Name            string            `json:"name,omitempty"`
	DiscoverySource ClusterSource     `json:"discoverySource"`
	PodName         string            `json:"podName,omitempty"`
	Namespace       string            `json:"namespace,omitempty"`
	MetricsURL      string            `json:"metricsUrl,omitempty"`
	MetricsPort     int               `json:"metricsPort,omitempty"`
	SourceFiler     string            `json:"sourceFiler"`
	SourcePath      string            `json:"sourcePath,omitempty"`
	TargetFiler     string            `json:"targetFiler"`
	TargetPath      string            `json:"targetPath,omitempty"`
	IsActivePassive bool              `json:"isActivePassive"`
	Labels          map[string]string `json:"labels,omitempty"`
}

// ClusterTarget represents a unified SeaweedFS cluster target.
type ClusterTarget struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Namespace    string            `json:"namespace,omitempty"`
	Source       ClusterSource     `json:"source"`
	Masters      []string          `json:"masters"`
	ActiveLeader string            `json:"activeLeader,omitempty"`
	Filer        string            `json:"filer,omitempty"`
	S3           string            `json:"s3,omitempty"`
	TLS          config.TLSConfig  `json:"-"`
	TLSEnabled   bool              `json:"tlsEnabled"`
	FilerSyncs   []FilerSyncTarget `json:"filerSyncs,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	Health       *ClusterHealth    `json:"health,omitempty"`
}
