package cluster

import "context"

// ClusterProvider retrieves SeaweedFS clusters from an underlying source.
type ClusterProvider interface {
	ListClusters(ctx context.Context) ([]ClusterTarget, error)
}

// Registry manages the unified catalog of clusters with thread-safe access.
type Registry interface {
	ClusterProvider
	GetCluster(ctx context.Context, id string) (*ClusterTarget, error)
	Refresh(ctx context.Context) error
	Start(ctx context.Context) error
	Stop()
}
