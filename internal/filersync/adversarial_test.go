package filersync

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"seaweedfs-ui/internal/cluster"
)

// TestAdv_PrometheusParser_MalformedText tests parser robustness against malformed lines.
func TestAdv_PrometheusParser_MalformedText(t *testing.T) {
	input := `
# HELP SeaweedFS_filerSync_sync_offset Last synced offset timestamp
# TYPE SeaweedFS_filerSync_sync_offset gauge
SeaweedFS_filerSync_sync_offset
SeaweedFS_filerSync_sync_offset not_a_number
   
   # Another comment
	
SeaweedFS_filerSync_synced_files_total 42
trailing_garbage_line_with_many_words one two three
SeaweedFS_filerSync_bytes_per_second 123.456
`
	metrics := parsePrometheusMetrics(strings.NewReader(input))

	if len(metrics) != 2 {
		t.Fatalf("expected exactly 2 valid parsed metrics, got %d: %+v", len(metrics), metrics)
	}
	if metrics["SeaweedFS_filerSync_synced_files_total"] != 42 {
		t.Errorf("expected synced_files_total 42, got %v", metrics["SeaweedFS_filerSync_synced_files_total"])
	}
	if metrics["SeaweedFS_filerSync_bytes_per_second"] != 123.456 {
		t.Errorf("expected bytes_per_second 123.456, got %v", metrics["SeaweedFS_filerSync_bytes_per_second"])
	}
}

// TestAdv_PrometheusParser_ScientificNotation verifies parsing of values in scientific notation.
func TestAdv_PrometheusParser_ScientificNotation(t *testing.T) {
	input := `
SeaweedFS_filerSync_sync_offset 1.72918e+09
SeaweedFS_filerSync_bytes_per_second 2.5e+06
SeaweedFS_filerSync_synced_files_total 1.5e+05
SeaweedFS_filerSync_sync_errors_total 0e+00
`
	metrics := parsePrometheusMetrics(strings.NewReader(input))

	if metrics["SeaweedFS_filerSync_sync_offset"] != 1.72918e+09 {
		t.Errorf("expected 1.72918e+09, got %v", metrics["SeaweedFS_filerSync_sync_offset"])
	}
	if metrics["SeaweedFS_filerSync_bytes_per_second"] != 2.5e+06 {
		t.Errorf("expected 2.5e+06, got %v", metrics["SeaweedFS_filerSync_bytes_per_second"])
	}
	if metrics["SeaweedFS_filerSync_synced_files_total"] != 150000 {
		t.Errorf("expected 150000, got %v", metrics["SeaweedFS_filerSync_synced_files_total"])
	}
	if metrics["SeaweedFS_filerSync_sync_errors_total"] != 0 {
		t.Errorf("expected 0, got %v", metrics["SeaweedFS_filerSync_sync_errors_total"])
	}
}

// TestAdv_PrometheusParser_Labels tests parsing with, without, and spaces in labels.
func TestAdv_PrometheusParser_Labels(t *testing.T) {
	input := `
# Without any labels
SeaweedFS_filerSync_sync_offset 1729180000
# With labels
SeaweedFS_filerSync_synced_files_total{source="filerA",target="filerB"} 500
# Label with comma and no spaces
SeaweedFS_filerSync_bytes_per_second{path="/buckets/data",active="true"} 88.5
`
	metrics := parsePrometheusMetrics(strings.NewReader(input))

	if metrics["SeaweedFS_filerSync_sync_offset"] != 1729180000 {
		t.Errorf("expected 1729180000 without labels, got %v", metrics["SeaweedFS_filerSync_sync_offset"])
	}
	if metrics["SeaweedFS_filerSync_synced_files_total"] != 500 {
		t.Errorf("expected 500 with labels, got %v", metrics["SeaweedFS_filerSync_synced_files_total"])
	}
	if metrics["SeaweedFS_filerSync_bytes_per_second"] != 88.5 {
		t.Errorf("expected 88.5 with labels, got %v", metrics["SeaweedFS_filerSync_bytes_per_second"])
	}

	// Adversarial test: labels with internal space (e.g. source="filer a")
	// strings.Fields splits on the space, causing parts[1] to be "a\",target=\"filerB\"}" instead of the value
	spaceInput := `SeaweedFS_filerSync_synced_files_total{source="filer a",target="filerB"} 500`
	spaceMetrics := parsePrometheusMetrics(strings.NewReader(spaceInput))
	if _, ok := spaceMetrics["SeaweedFS_filerSync_synced_files_total"]; !ok {
		t.Log("Note (Finding): Prometheus metric with space inside label value was discarded by strings.Fields")
	}
}

// TestAdv_PrometheusParser_NaNAndInfEmpirical demonstrates the exact behavior when NaN/Inf are parsed.
func TestAdv_PrometheusParser_NaNAndInfEmpirical(t *testing.T) {
	input := `
SeaweedFS_filerSync_sync_offset NaN
SeaweedFS_filerSync_bytes_per_second NaN
SeaweedFS_filerSync_synced_files_total +Inf
`
	metrics := parsePrometheusMetrics(strings.NewReader(input))

	if !math.IsNaN(metrics["SeaweedFS_filerSync_sync_offset"]) {
		t.Errorf("expected NaN for sync_offset, got %v", metrics["SeaweedFS_filerSync_sync_offset"])
	}
	if !math.IsNaN(metrics["SeaweedFS_filerSync_bytes_per_second"]) {
		t.Errorf("expected NaN for bytes_per_second, got %v", metrics["SeaweedFS_filerSync_bytes_per_second"])
	}
	if !math.IsInf(metrics["SeaweedFS_filerSync_synced_files_total"], 1) {
		t.Errorf("expected +Inf for synced_files_total, got %v", metrics["SeaweedFS_filerSync_synced_files_total"])
	}

	// Now check computeLag with NaN
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	lag, ts := computeLag(metrics["SeaweedFS_filerSync_sync_offset"], now)

	// Since NaN <= 0 is false, computeLag converts int64(NaN) which evaluates to 0 (1970-01-01),
	// resulting in a pseudo-lag of 1.78 billion seconds (~56 years)
	t.Logf("Empirical Observation: computeLag(NaN) produces lag=%.2f seconds (%.1f years), ts=%d", lag, lag/(365.25*86400), ts)

	// Check JSON marshaling of FilerSyncStatus when BytesPerSecond is NaN
	status := SyncStatus{
		SourceFiler:    "f1:8888",
		TargetFiler:    "f2:8888",
		Status:         StatusHealthy,
		BytesPerSecond: metrics["SeaweedFS_filerSync_bytes_per_second"], // NaN
	}
	apiModel := status.ToFilerSyncStatus("s1", "prod-to-dr", "kubernetes")
	_, jsonErr := json.Marshal(apiModel)
	if jsonErr != nil {
		t.Logf("Empirical Finding: json.Marshal fails with error when float64 field is NaN: %v", jsonErr)
	}
}

// TestAdv_ClockSkew_FutureLastSync verifies that when lastSynced is in the future due to NTP drift,
// max(0, now - lastSyncedAt) returns 0.0s without underflow.
func TestAdv_ClockSkew_FutureLastSync(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

	// 1. lastSynced 15 seconds in future (seconds scale)
	futureSec := float64(now.Add(15 * time.Second).Unix())
	lag, ts := computeLag(futureSec, now)
	if lag != 0.0 {
		t.Errorf("expected lag 0.0 for future timestamp, got %f", lag)
	}
	if ts != now.Add(15*time.Second).Unix() {
		t.Errorf("expected future timestamp preserved in ts, got %d", ts)
	}

	// 2. lastSynced 500ms in future (milliseconds scale)
	futureMs := float64(now.Add(500 * time.Millisecond).UnixMilli())
	lagMs, _ := computeLag(futureMs, now)
	if lagMs != 0.0 {
		t.Errorf("expected lag 0.0 for future ms timestamp, got %f", lagMs)
	}

	// 3. lastSynced 1 minute in future (nanoseconds scale)
	futureNs := float64(now.Add(1 * time.Minute).UnixNano())
	lagNs, _ := computeLag(futureNs, now)
	if lagNs != 0.0 {
		t.Errorf("expected lag 0.0 for future ns timestamp, got %f", lagNs)
	}

	// 4. lastSynced far in the future (year 2050)
	farFuture := float64(time.Date(2050, 1, 1, 0, 0, 0, 0, time.UTC).Unix())
	lagFar, _ := computeLag(farFuture, now)
	if lagFar != 0.0 {
		t.Errorf("expected lag 0.0 for far future timestamp, got %f", lagFar)
	}
}

// TestAdv_ClockSkew_ZeroAndNegativeOffset verifies handling of zero and negative offsets.
func TestAdv_ClockSkew_ZeroAndNegativeOffset(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

	// 0 offset
	lag0, ts0 := computeLag(0, now)
	if lag0 != 0.0 || ts0 != 0 {
		t.Errorf("expected (0.0, 0) for offset 0, got (%f, %d)", lag0, ts0)
	}

	// Negative offset
	lagNeg, tsNeg := computeLag(-500.0, now)
	if lagNeg != 0.0 || tsNeg != 0 {
		t.Errorf("expected (0.0, 0) for negative offset, got (%f, %d)", lagNeg, tsNeg)
	}
}

// TestAdv_Inspector_UnreachableMetricEndpoint tests scraping unreachable endpoints.
func TestAdv_Inspector_UnreachableMetricEndpoint(t *testing.T) {
	net := newTestNetwork()

	insp := NewInspector(10*time.Second, 60*time.Second, 500*time.Millisecond)
	insp.SetDialContext(net.DialContext)

	target := cluster.ClusterTarget{
		ID: "test-unreachable-sync",
		FilerSyncs: []cluster.FilerSyncTarget{
			{
				ID:          "sync-unreachable",
				PodName:     "dead-pod",
				MetricsPort: 9327,
				SourceFiler: "filer1:8888",
				TargetFiler: "filer2:8888",
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	results, err := insp.InspectSyncJobs(ctx, target)
	duration := time.Since(start)

	if err != nil {
		t.Fatalf("InspectSyncJobs should return results with StatusDegraded, not fail: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Status != StatusDegraded {
		t.Errorf("expected StatusDegraded for unreachable endpoint, got %s", results[0].Status)
	}
	if duration > 1500*time.Millisecond {
		t.Errorf("unreachable endpoint probe took too long (%v)", duration)
	}
}

// TestAdv_Inspector_ConcurrentStress runs 20 simultaneous scrapings with varied latency and errors.
func TestAdv_Inspector_ConcurrentStress(t *testing.T) {
	net := newTestNetwork()

	const numTargets = 20
	syncTargets := make([]cluster.FilerSyncTarget, numTargets)

	for i := 0; i < numTargets; i++ {
		podAddr := fmt.Sprintf("sync-stress-pod-%d:9327", i)
		idx := i
		_, cleanup := net.NewServer(podAddr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if idx%3 == 0 {
				// Simulate internal server error
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			_, _ = fmt.Fprintf(w, "SeaweedFS_filerSync_sync_offset %d\n", time.Now().Unix()-int64(idx*2))
			_, _ = fmt.Fprintf(w, "SeaweedFS_filerSync_synced_files_total %d\n", idx*100)
			_, _ = fmt.Fprintf(w, "SeaweedFS_filerSync_bytes_per_second %f\n", float64(idx)*10.5)
			_, _ = fmt.Fprintf(w, "SeaweedFS_filerSync_sync_errors_total %d\n", idx%2)
		}))
		defer cleanup()

		syncTargets[i] = cluster.FilerSyncTarget{
			ID:          fmt.Sprintf("sync-%d", i),
			PodName:     fmt.Sprintf("sync-stress-pod-%d", i),
			MetricsPort: 9327,
			SourceFiler: "filerA:8888",
			TargetFiler: "filerB:8888",
		}
	}

	insp := NewInspector(10*time.Second, 60*time.Second, 1*time.Second)
	insp.SetDialContext(net.DialContext)

	target := cluster.ClusterTarget{
		ID:         "test-stress-sync",
		FilerSyncs: syncTargets,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	results, err := insp.InspectSyncJobs(ctx, target)
	if err != nil {
		t.Fatalf("InspectSyncJobs failed under concurrent stress: %v", err)
	}
	if len(results) != numTargets {
		t.Fatalf("expected %d results, got %d", numTargets, len(results))
	}

	for i, res := range results {
		if i%3 == 0 {
			if res.Status != StatusDegraded {
				t.Errorf("target %d expected StatusDegraded on HTTP 500, got %s", i, res.Status)
			}
		} else {
			if res.Status == "" {
				t.Errorf("target %d had empty status", i)
			}
		}
	}
}
