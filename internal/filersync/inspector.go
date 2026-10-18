package filersync

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"seaweedfs-ui/internal/cluster"
	"seaweedfs-ui/internal/pki"
)

// Inspector inspects running weed filer.sync replication jobs.
type Inspector interface {
	InspectSyncJobs(ctx context.Context, target cluster.ClusterTarget) ([]SyncStatus, error)
}

// DefaultInspector implements Inspector with concurrent scraping and zero-dependency Prometheus parsing.
type DefaultInspector struct {
	mu            sync.RWMutex
	warnThreshold time.Duration
	critThreshold time.Duration
	scrapeTimeout time.Duration
	dialContext   func(ctx context.Context, network, addr string) (net.Conn, error)
	nowFunc       func() time.Time
}

// NewInspector constructs a DefaultInspector.
func NewInspector(warnThreshold, critThreshold, scrapeTimeout time.Duration) *DefaultInspector {
	if warnThreshold <= 0 {
		warnThreshold = 10 * time.Second
	}
	if critThreshold <= 0 {
		critThreshold = 60 * time.Second
	}
	if scrapeTimeout <= 0 {
		scrapeTimeout = 3 * time.Second
	}
	return &DefaultInspector{
		warnThreshold: warnThreshold,
		critThreshold: critThreshold,
		scrapeTimeout: scrapeTimeout,
		nowFunc:       time.Now,
	}
}

// SetDialContext sets a custom network dialer, supporting in-memory pipe listeners.
func (insp *DefaultInspector) SetDialContext(fn func(ctx context.Context, network, addr string) (net.Conn, error)) {
	insp.mu.Lock()
	defer insp.mu.Unlock()
	insp.dialContext = fn
}

// SetNowFunc overrides the clock for deterministic testing.
func (insp *DefaultInspector) SetNowFunc(fn func() time.Time) {
	insp.mu.Lock()
	defer insp.mu.Unlock()
	insp.nowFunc = fn
}

func (insp *DefaultInspector) getNow() time.Time {
	insp.mu.RLock()
	defer insp.mu.RUnlock()
	if insp.nowFunc != nil {
		return insp.nowFunc().UTC()
	}
	return time.Now().UTC()
}

// InspectSyncJobs scrapes Prometheus endpoints and calculates replication lag and health status.
func (insp *DefaultInspector) InspectSyncJobs(ctx context.Context, target cluster.ClusterTarget) ([]SyncStatus, error) {
	if len(target.FilerSyncs) == 0 {
		return []SyncStatus{}, nil
	}

	// Prepare HTTP client with target TLS configuration
	tlsConfig, err := pki.BuildTLSConfig(target.TLS)
	if err != nil {
		return nil, fmt.Errorf("build TLS config: %w", err)
	}

	insp.mu.RLock()
	customDialer := insp.dialContext
	warnThresh := insp.warnThreshold
	critThresh := insp.critThreshold
	scrapeTimeout := insp.scrapeTimeout
	insp.mu.RUnlock()

	dialContext := customDialer
	if dialContext == nil {
		dialContext = (&net.Dialer{
			Timeout:   scrapeTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext
	}

	transport := &http.Transport{
		TLSClientConfig: tlsConfig,
		Proxy:           http.ProxyFromEnvironment,
		DialContext:     dialContext,
		MaxIdleConns:    100,
		IdleConnTimeout: 90 * time.Second,
	}

	httpClient := &http.Client{
		Transport: transport,
		Timeout:   scrapeTimeout,
	}

	now := insp.getNow()
	results := make([]SyncStatus, len(target.FilerSyncs))
	var wg sync.WaitGroup

	for i, syncJob := range target.FilerSyncs {
		wg.Add(1)
		go func(idx int, job cluster.FilerSyncTarget) {
			defer wg.Done()

			sourcePath := job.SourcePath
			if sourcePath == "" {
				sourcePath = "/"
			}
			targetPath := job.TargetPath
			if targetPath == "" {
				targetPath = "/"
			}
			mode := ModeActiveActive
			if job.IsActivePassive {
				mode = ModeActivePassive
			}

			// Baseline status struct
			status := SyncStatus{
				SourceFiler: job.SourceFiler,
				TargetFiler: job.TargetFiler,
				SourcePath:  sourcePath,
				TargetPath:  targetPath,
				Mode:        mode,
				PodName:     job.PodName,
				Namespace:   job.Namespace,
				Status:      StatusDegraded,
			}

			// Determine scrape URL
			metricsURL := job.MetricsURL
			if metricsURL == "" && job.MetricsPort > 0 && job.PodName != "" {
				scheme := "http"
				if target.TLS.Enabled {
					scheme = "https"
				}
				metricsURL = fmt.Sprintf("%s://%s:%d/metrics", scheme, job.PodName, job.MetricsPort)
			}

			// If no metrics endpoint is configured or available, return degraded status
			if metricsURL == "" {
				results[idx] = status
				return
			}

			// Check context cancellation
			if ctx.Err() != nil {
				status.Status = StatusError
				results[idx] = status
				return
			}

			scrapeCtx, cancel := context.WithTimeout(ctx, scrapeTimeout)
			defer cancel()

			req, err := http.NewRequestWithContext(scrapeCtx, http.MethodGet, metricsURL, nil)
			if err != nil {
				results[idx] = status
				return
			}

			resp, err := httpClient.Do(req)
			if err != nil {
				results[idx] = status
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				results[idx] = status
				return
			}

			// Limit metric stream to 512KB to protect memory
			lr := io.LimitReader(resp.Body, 512*1024)
			metrics := parsePrometheusMetrics(lr)

			// Extract key metrics
			var syncOffset float64
			var syncedFiles int64
			var bytesPerSec float64
			var syncErrors int64

			for k, v := range metrics {
				lower := strings.ToLower(k)
				if strings.Contains(lower, "sync_offset") {
					syncOffset = v
				} else if strings.Contains(lower, "synced_files_total") || strings.Contains(lower, "synced_file_count") {
					syncedFiles = int64(v)
				} else if strings.Contains(lower, "bytes_per_second") || strings.Contains(lower, "events_per_second") {
					bytesPerSec = v
				} else if strings.Contains(lower, "sync_errors_total") || strings.Contains(lower, "sync_error") {
					syncErrors = int64(v)
				}
			}

			lag, lastSyncTs := computeLag(syncOffset, now)

			status.ReplicationLagSeconds = lag
			status.LastSyncTimestamp = lastSyncTs
			status.SyncedFileCount = syncedFiles
			status.BytesPerSecond = bytesPerSec

			// Evaluate status state machine
			status.Status = evaluateStatus(syncErrors, lag, warnThresh.Seconds(), critThresh.Seconds())
			results[idx] = status
		}(i, syncJob)
	}

	wg.Wait()
	return results, nil
}

// computeLag calculates replication lag in seconds, handling ns, ms, and sec timestamp scales.
func computeLag(offsetVal float64, now time.Time) (float64, int64) {
	if offsetVal <= 0 {
		return 0.0, 0
	}

	var lastSynced time.Time
	if offsetVal > 1e16 {
		// Nanoseconds (e.g. 1729180000000000000)
		lastSynced = time.Unix(0, int64(offsetVal)).UTC()
	} else if offsetVal > 1e11 {
		// Milliseconds (e.g. 1729180000000)
		lastSynced = time.UnixMilli(int64(offsetVal)).UTC()
	} else {
		// Seconds (e.g. 1729180000)
		lastSynced = time.Unix(int64(offsetVal), 0).UTC()
	}

	lag := now.Sub(lastSynced).Seconds()
	// Clamp negative lag resulting from NTP clock drift
	lag = math.Max(0, lag)

	return lag, lastSynced.Unix()
}

// evaluateStatus determines health based on errors and replication lag.
func evaluateStatus(errorsTotal int64, lag float64, warnThresholdSec, critThresholdSec float64) string {
	if errorsTotal > 0 {
		return StatusError
	}
	if lag >= critThresholdSec {
		return StatusLagging
	}
	if lag >= warnThresholdSec {
		return StatusLagging
	}
	return StatusHealthy
}

// parsePrometheusMetrics parses Prometheus text exposition stream without external dependencies.
func parsePrometheusMetrics(r io.Reader) map[string]float64 {
	metrics := make(map[string]float64)
	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}

		metricName := parts[0]
		// Strip label section: metric_name{source="a",target="b"} -> metric_name
		if idx := strings.Index(metricName, "{"); idx != -1 {
			metricName = metricName[:idx]
		}

		valStr := parts[1]
		val, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			continue
		}

		metrics[metricName] = val
	}

	return metrics
}
