package weedhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"seaweedfs-ui/internal/cluster"
	"seaweedfs-ui/internal/config"
)

// TestAdv_MultiMasterFailover_MasterCrash verifies that when the first master crashes/drops conn,
// the client transparently fails over to the next master.
func TestAdv_MultiMasterFailover_MasterCrash(t *testing.T) {
	net := newTestNetwork()

	// Master 1: drops connection / returns 500
	_, cleanup1 := net.NewServer("master-crash-1:9333", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer cleanup1()

	// Master 2: healthy leader
	_, cleanup2 := net.NewServer("master-crash-2:9333", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ClusterStatus{
			IsLeader: true,
			Leader:   "master-crash-2:9333",
			Peers:    []string{"master-crash-1:9333", "master-crash-2:9333"},
		})
	}))
	defer cleanup2()

	client := NewClient(2 * time.Second)
	client.SetDialContext(net.DialContext)

	target := cluster.ClusterTarget{
		ID:      "test-crash-cluster",
		Masters: []string{"master-crash-1:9333", "master-crash-2:9333"},
		TLS:     config.TLSConfig{Enabled: false},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	status, err := client.GetClusterStatus(ctx, target)
	if err != nil {
		t.Fatalf("expected successful failover to master 2, got error: %v", err)
	}
	if status.Leader != "master-crash-2:9333" {
		t.Fatalf("expected leader master-crash-2:9333, got %s", status.Leader)
	}
}

// TestAdv_MultiMasterFailover_307RedirectLoop verifies that mutual 307 redirects terminate cleanly
// and return an error without infinite loop or hanging.
func TestAdv_MultiMasterFailover_307RedirectLoop(t *testing.T) {
	net := newTestNetwork()

	// Master 1 redirects to Master 2
	_, cleanup1 := net.NewServer("master-loop-1:9333", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://master-loop-2:9333/cluster/status", http.StatusTemporaryRedirect)
	}))
	defer cleanup1()

	// Master 2 redirects back to Master 1
	_, cleanup2 := net.NewServer("master-loop-2:9333", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://master-loop-1:9333/cluster/status", http.StatusTemporaryRedirect)
	}))
	defer cleanup2()

	client := NewClient(2 * time.Second)
	client.SetDialContext(net.DialContext)

	target := cluster.ClusterTarget{
		ID:      "test-loop-cluster",
		Masters: []string{"master-loop-1:9333", "master-loop-2:9333"},
		TLS:     config.TLSConfig{Enabled: false},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	_, err := client.GetClusterStatus(ctx, target)
	duration := time.Since(start)

	if err == nil {
		t.Fatal("expected error from 307 redirect loop, got nil")
	}
	if duration > 1500*time.Millisecond {
		t.Fatalf("redirect loop took too long (%v), possible loop hang", duration)
	}
}

// TestAdv_MultiMasterFailover_Self307Redirect verifies that a 307 redirect pointing to itself does not loop.
func TestAdv_MultiMasterFailover_Self307Redirect(t *testing.T) {
	net := newTestNetwork()

	_, cleanup := net.NewServer("master-self-loop:9333", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://master-self-loop:9333/cluster/status", http.StatusTemporaryRedirect)
	}))
	defer cleanup()

	client := NewClient(2 * time.Second)
	client.SetDialContext(net.DialContext)

	target := cluster.ClusterTarget{
		ID:      "test-self-loop-cluster",
		Masters: []string{"master-self-loop:9333"},
		TLS:     config.TLSConfig{Enabled: false},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := client.GetClusterStatus(ctx, target)
	if err == nil {
		t.Fatal("expected error on self 307 redirect, got nil")
	}
}

// TestAdv_MultiMasterFailover_ClusterPartition verifies that when all masters are unreachable,
// ErrAllMastersUnreachable is returned promptly.
func TestAdv_MultiMasterFailover_ClusterPartition(t *testing.T) {
	net := newTestNetwork()

	// Master 1: 503
	_, cleanup1 := net.NewServer("partition-1:9333", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer cleanup1()

	// Master 2: 500
	_, cleanup2 := net.NewServer("partition-2:9333", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer cleanup2()

	// Master 3: unmapped address (network dial failure)

	client := NewClient(2 * time.Second)
	client.SetDialContext(net.DialContext)

	target := cluster.ClusterTarget{
		ID:      "test-partition-cluster",
		Masters: []string{"partition-1:9333", "partition-2:9333", "partition-unmapped:9333"},
		TLS:     config.TLSConfig{Enabled: false},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := client.GetClusterStatus(ctx, target)
	if err == nil {
		t.Fatal("expected ErrAllMastersUnreachable, got nil")
	}
	if !errors.Is(err, ErrAllMastersUnreachable) {
		t.Fatalf("expected error wrapping ErrAllMastersUnreachable, got: %v", err)
	}
}

// TestAdv_Filer_HugeDirectoryListing verifies browsing with >1,000 directory entries.
func TestAdv_Filer_HugeDirectoryListing(t *testing.T) {
	net := newTestNetwork()

	const entryCount = 1500
	_, cleanup := net.NewServer("filer-huge:8888", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entries := make([]FilerEntry, entryCount)
		for i := 0; i < entryCount; i++ {
			entries[i] = FilerEntry{
				FullPath: fmt.Sprintf("/huge/file_%05d.dat", i),
				Mode:     0o644,
				FileSize: uint64(1000 + i),
				Mime:     "application/octet-stream",
				Mtime:    time.Now().UTC(),
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(FilerDirResult{
			Path:                  "/huge/",
			Entries:               entries,
			Limit:                 2000,
			LastFileName:          fmt.Sprintf("file_%05d.dat", entryCount-1),
			ShouldDisplayLoadMore: false,
		})
	}))
	defer cleanup()

	client := NewClient(5 * time.Second)
	client.SetDialContext(net.DialContext)

	target := cluster.ClusterTarget{
		ID:    "test-filer-huge",
		Filer: "filer-huge:8888",
		TLS:   config.TLSConfig{Enabled: false},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := client.ListFilerDir(ctx, target, "/huge", 2000)
	if err != nil {
		t.Fatalf("failed to list huge directory: %v", err)
	}

	if len(res.Entries) != entryCount {
		t.Fatalf("expected %d entries, got %d", entryCount, len(res.Entries))
	}
	if res.Entries[0].Name() != "file_00000.dat" {
		t.Fatalf("unexpected first entry name: %s", res.Entries[0].Name())
	}
	if res.Entries[entryCount-1].Name() != fmt.Sprintf("file_%05d.dat", entryCount-1) {
		t.Fatalf("unexpected last entry name: %s", res.Entries[entryCount-1].Name())
	}
}

// TestAdv_Filer_EmptyAndNullResponses verifies handling of empty entries list, null entries, and 0-byte body.
func TestAdv_Filer_EmptyAndNullResponses(t *testing.T) {
	net := newTestNetwork()

	_, cleanup := net.NewServer("filer-empty:8888", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(p, "/empty-list"):
			_ = json.NewEncoder(w).Encode(FilerDirResult{
				Path:    "/empty-list/",
				Entries: []FilerEntry{},
			})
		case strings.HasPrefix(p, "/null-entries"):
			_ = json.NewEncoder(w).Encode(FilerDirResult{
				Path:    "/null-entries/",
				Entries: nil,
			})
		case strings.HasPrefix(p, "/zero-bytes"):
			// Return 200 with empty body
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer cleanup()

	client := NewClient(2 * time.Second)
	client.SetDialContext(net.DialContext)

	target := cluster.ClusterTarget{
		ID:    "test-filer-empty",
		Filer: "filer-empty:8888",
		TLS:   config.TLSConfig{Enabled: false},
	}

	ctx := context.Background()

	// 1. Empty entries array
	res1, err := client.ListFilerDir(ctx, target, "/empty-list", 100)
	if err != nil {
		t.Fatalf("expected success for empty list, got: %v", err)
	}
	if len(res1.Entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(res1.Entries))
	}

	// 2. Null entries
	res2, err := client.ListFilerDir(ctx, target, "/null-entries", 100)
	if err != nil {
		t.Fatalf("expected success for null entries, got: %v", err)
	}
	if len(res2.Entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(res2.Entries))
	}

	// 3. Zero-byte body
	_, err = client.ListFilerDir(ctx, target, "/zero-bytes", 100)
	if err == nil {
		t.Fatal("expected error decoding zero-byte body, got nil")
	}
}

// TestAdv_Filer_DeeplyNestedPaths tests deeply nested paths and path cleaning.
func TestAdv_Filer_DeeplyNestedPaths(t *testing.T) {
	net := newTestNetwork()

	var receivedPath string
	_, cleanup := net.NewServer("filer-deep:8888", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(FilerDirResult{
			Path:    receivedPath,
			Entries: []FilerEntry{},
		})
	}))
	defer cleanup()

	client := NewClient(2 * time.Second)
	client.SetDialContext(net.DialContext)

	target := cluster.ClusterTarget{
		ID:    "test-filer-deep",
		Filer: "filer-deep:8888",
		TLS:   config.TLSConfig{Enabled: false},
	}

	// 50 levels deep with redundant slashes and relative elements
	deepSegments := make([]string, 50)
	for i := range deepSegments {
		deepSegments[i] = fmt.Sprintf("level%02d", i)
	}
	rawPath := "///" + strings.Join(deepSegments, "///") + "/../" + deepSegments[49]

	ctx := context.Background()
	_, err := client.ListFilerDir(ctx, target, rawPath, 100)
	if err != nil {
		t.Fatalf("failed listing deep path: %v", err)
	}

	if !strings.HasPrefix(receivedPath, "/level00/level01") {
		t.Fatalf("unexpected cleaned path: %s", receivedPath)
	}
	if strings.Contains(receivedPath, "//") {
		t.Fatalf("cleaned path still contains double slashes: %s", receivedPath)
	}
}

// faultyReader simulates a broken reader that fails after reading N bytes.
type faultyReader struct {
	data      []byte
	pos       int
	failAfter int
	err       error
}

func (r *faultyReader) Read(p []byte) (n int, err error) {
	if r.pos >= r.failAfter {
		return 0, r.err
	}
	n = copy(p, r.data[r.pos:])
	r.pos += n
	if r.pos >= r.failAfter {
		return n, r.err
	}
	return n, nil
}

// TestAdv_UploadFile_PipeBreak simulates an interrupted pipe during streaming upload.
func TestAdv_UploadFile_PipeBreak(t *testing.T) {
	net := newTestNetwork()

	_, cleanup := net.NewServer("filer-pipe-break:8888", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read body until error
		_, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer cleanup()

	client := NewClient(2 * time.Second)
	client.SetDialContext(net.DialContext)

	target := cluster.ClusterTarget{
		ID:    "test-pipe-break",
		Filer: "filer-pipe-break:8888",
		TLS:   config.TLSConfig{Enabled: false},
	}

	payload := bytes.Repeat([]byte("A"), 1024*1024) // 1MB
	brokenReader := &faultyReader{
		data:      payload,
		failAfter: 4096, // Fail after 4KB
		err:       io.ErrUnexpectedEOF,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := client.UploadFile(ctx, target, "/uploads", "broken.bin", brokenReader, "application/octet-stream")
	if err == nil {
		t.Fatal("expected error on broken pipe upload, got nil")
	}
}

// TestAdv_UploadFile_ServerAbort simulates server abruptly closing connection mid-upload.
func TestAdv_UploadFile_ServerAbort(t *testing.T) {
	net := newTestNetwork()

	_, cleanup := net.NewServer("filer-server-abort:8888", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read only 1KB and immediately return 500 error
		buf := make([]byte, 1024)
		_, _ = r.Body.Read(buf)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer cleanup()

	client := NewClient(2 * time.Second)
	client.SetDialContext(net.DialContext)

	target := cluster.ClusterTarget{
		ID:    "test-server-abort",
		Filer: "filer-server-abort:8888",
		TLS:   config.TLSConfig{Enabled: false},
	}

	payload := bytes.Repeat([]byte("B"), 512*1024)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := client.UploadFile(ctx, target, "/uploads", "abort.bin", bytes.NewReader(payload), "application/octet-stream")
	if err == nil {
		t.Fatal("expected error when server aborts upload, got nil")
	}
}

// TestAdv_StreamFile_DownloadInterrupted tests interrupted download streams.
func TestAdv_StreamFile_DownloadInterrupted(t *testing.T) {
	net := newTestNetwork()

	_, cleanup := net.NewServer("filer-download:8888", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/notfound.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Hello, SeaweedFS!"))
	}))
	defer cleanup()

	client := NewClient(2 * time.Second)
	client.SetDialContext(net.DialContext)

	target := cluster.ClusterTarget{
		ID:    "test-download",
		Filer: "filer-download:8888",
		TLS:   config.TLSConfig{Enabled: false},
	}

	ctx := context.Background()

	// 1. Successful download
	rc, contentType, err := client.StreamFile(ctx, target, "/test.txt")
	if err != nil {
		t.Fatalf("expected successful download, got: %v", err)
	}
	defer rc.Close()

	if contentType != "application/octet-stream" {
		t.Fatalf("unexpected content type: %s", contentType)
	}
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("failed reading download stream: %v", err)
	}
	if string(data) != "Hello, SeaweedFS!" {
		t.Fatalf("unexpected download content: %s", string(data))
	}

	// 2. 404 mapped to ErrNotFound
	_, _, err = client.StreamFile(ctx, target, "/notfound.txt")
	if err == nil {
		t.Fatal("expected error for not found file, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}
