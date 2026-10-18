package filersync

import (
	"time"
)

// Operational Modes
const (
	ModeActiveActive  = "Active-Active"
	ModeActivePassive = "Active-Passive"
)

// Sync Health Statuses
const (
	StatusHealthy  = "HEALTHY"
	StatusDegraded = "DEGRADED"
	StatusLagging  = "LAGGING"
	StatusError    = "ERROR"
)

// SyncStatus represents the domain model for an observed filer.sync replication job.
type SyncStatus struct {
	SourceFiler           string  `json:"sourceFiler"`
	TargetFiler           string  `json:"targetFiler"`
	SourcePath            string  `json:"sourcePath"`
	TargetPath            string  `json:"targetPath"`
	Mode                  string  `json:"mode"`
	PodName               string  `json:"podName"`
	Namespace             string  `json:"namespace"`
	ReplicationLagSeconds float64 `json:"replicationLagSeconds"`
	SyncedFileCount       int64   `json:"syncedFileCount"`
	BytesPerSecond        float64 `json:"bytesPerSecond"`
	LastSyncTimestamp     int64   `json:"lastSyncTimestamp"`
	Status                string  `json:"status"`
}

// FilerSyncStatus provides the nested REST API response model matching SPEC.md §4.6.
type FilerSyncStatus struct {
	ID              string              `json:"id"`
	Name            string              `json:"name"`
	DiscoverySource string              `json:"discoverySource"`
	PodName         string              `json:"podName"`
	Namespace       string              `json:"namespace"`
	State           string              `json:"state"`
	Ready           bool                `json:"ready"`
	Restarts        int                 `json:"restarts"`
	UptimeSeconds   int64               `json:"uptimeSeconds"`
	Mode            string              `json:"mode"`
	Source          FilerEndpoint       `json:"source"`
	Target          FilerEndpoint       `json:"target"`
	Metrics         FilerSyncMetrics    `json:"metrics"`
	Checkpoint      FilerSyncCheckpoint `json:"checkpoint"`
}

type FilerEndpoint struct {
	Filer string `json:"filer"`
	Path  string `json:"path"`
}

type FilerSyncMetrics struct {
	Available        bool    `json:"available"`
	SyncedFilesTotal int64   `json:"syncedFilesTotal"`
	SyncErrorsTotal  int64   `json:"syncErrorsTotal"`
	EventsPerSecond  float64 `json:"eventsPerSecond"`
	Goroutines       int     `json:"goroutines"`
	MemoryBytes      uint64  `json:"memoryBytes"`
}

type FilerSyncCheckpoint struct {
	LastSyncedAt          string  `json:"lastSyncedAt"`
	ReplicationLagSeconds float64 `json:"replicationLagSeconds"`
}

// ToFilerSyncStatus maps a domain SyncStatus to the nested REST API model.
func (s SyncStatus) ToFilerSyncStatus(id, name, source string) FilerSyncStatus {
	ready := s.Status == StatusHealthy || s.Status == StatusLagging
	lastSyncedStr := ""
	if s.LastSyncTimestamp > 0 {
		lastSyncedStr = time.Unix(s.LastSyncTimestamp, 0).UTC().Format(time.RFC3339)
	}

	return FilerSyncStatus{
		ID:              id,
		Name:            name,
		DiscoverySource: source,
		PodName:         s.PodName,
		Namespace:       s.Namespace,
		State:           s.Status,
		Ready:           ready,
		Mode:            s.Mode,
		Source: FilerEndpoint{
			Filer: s.SourceFiler,
			Path:  s.SourcePath,
		},
		Target: FilerEndpoint{
			Filer: s.TargetFiler,
			Path:  s.TargetPath,
		},
		Metrics: FilerSyncMetrics{
			Available:        s.Status != StatusDegraded && s.Status != StatusError,
			SyncedFilesTotal: s.SyncedFileCount,
			EventsPerSecond:  s.BytesPerSecond,
		},
		Checkpoint: FilerSyncCheckpoint{
			LastSyncedAt:          lastSyncedStr,
			ReplicationLagSeconds: s.ReplicationLagSeconds,
		},
	}
}
