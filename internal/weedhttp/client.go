package weedhttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"seaweedfs-ui/internal/cluster"
	"seaweedfs-ui/internal/pki"
)

var (
	ErrNoMastersConfigured   = errors.New("cluster has no masters configured")
	ErrAllMastersUnreachable = errors.New("all configured masters are unreachable")
	ErrFilerNotConfigured    = errors.New("cluster has no filer endpoint configured")
	ErrNotFound              = errors.New("resource not found")
	ErrForbidden             = errors.New("access forbidden")
	ErrBadResponse           = errors.New("bad response from SeaweedFS endpoint")
)

// Client defines the HTTP/HTTPS interface for querying SeaweedFS daemons.
type Client interface {
	GetClusterStatus(ctx context.Context, target cluster.ClusterTarget) (*ClusterStatus, error)
	GetTopology(ctx context.Context, target cluster.ClusterTarget) (*Topology, error)
	GetVolumes(ctx context.Context, target cluster.ClusterTarget) ([]VolumeInfo, error)
	ListFilerDir(ctx context.Context, target cluster.ClusterTarget, path string, limit int) (*FilerDirResult, error)
	StreamFile(ctx context.Context, target cluster.ClusterTarget, path string) (io.ReadCloser, string, error)
	DeleteEntry(ctx context.Context, target cluster.ClusterTarget, path string, isDir bool) error

	// Operational and auxiliary probes
	GetDirStatus(ctx context.Context, target cluster.ClusterTarget) (*DirStatusResponse, error)
	GetVolumeStatus(ctx context.Context, target cluster.ClusterTarget) (*VolumeStatusResponse, error)
	UploadFile(ctx context.Context, target cluster.ClusterTarget, dirPath string, fileName string, reader io.Reader, mimeType string) error
	CheckS3Status(ctx context.Context, target cluster.ClusterTarget) (*S3EndpointStatus, error)
	GetVolumeServerStatus(ctx context.Context, target cluster.ClusterTarget, volumeServerAddr string) (*VolumeServerStatus, error)
}

// DefaultClient implements Client with http.Client pooling and multi-master failover.
type DefaultClient struct {
	mu           sync.RWMutex
	httpClients  map[string]*http.Client
	leaderCache  map[string]string // clusterID -> last known leader address
	queryTimeout time.Duration
	dialContext  func(ctx context.Context, network, addr string) (net.Conn, error)
}

// NewClient constructs a new DefaultClient.
func NewClient(timeout time.Duration) *DefaultClient {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &DefaultClient{
		httpClients:  make(map[string]*http.Client),
		leaderCache:  make(map[string]string),
		queryTimeout: timeout,
	}
}

// SetDialContext sets a custom network dialer, allowing in-memory pipe listeners in testing environments.
func (c *DefaultClient) SetDialContext(fn func(ctx context.Context, network, addr string) (net.Conn, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dialContext = fn
	c.httpClients = make(map[string]*http.Client)
}

// getHTTPClient retrieves or creates an http.Client configured with target's TLS settings.
func (c *DefaultClient) getHTTPClient(target cluster.ClusterTarget) (*http.Client, error) {
	c.mu.RLock()
	client, exists := c.httpClients[target.ID]
	c.mu.RUnlock()
	if exists {
		return client, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check write lock
	if client, exists = c.httpClients[target.ID]; exists {
		return client, nil
	}

	tlsConfig, err := pki.BuildTLSConfig(target.TLS)
	if err != nil {
		return nil, fmt.Errorf("build TLS config for cluster %q: %w", target.ID, err)
	}

	dialContext := c.dialContext
	if dialContext == nil {
		dialContext = (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
	}

	transport := &http.Transport{
		TLSClientConfig: tlsConfig,
		Proxy:           http.ProxyFromEnvironment,
		DialContext:     dialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	client = &http.Client{
		Transport: transport,
		Timeout:   c.queryTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Do not follow redirects automatically for manual 307 leader failover
			return http.ErrUseLastResponse
		},
	}

	c.httpClients[target.ID] = client
	return client, nil
}

// buildURL formats scheme and host:port cleanly.
func buildURL(tlsEnabled bool, addr, requestPath string) string {
	scheme := "http"
	if tlsEnabled {
		scheme = "https"
	}
	addr = strings.TrimPrefix(addr, "http://")
	addr = strings.TrimPrefix(addr, "https://")
	addr = strings.TrimRight(addr, "/")

	if !strings.HasPrefix(requestPath, "/") {
		requestPath = "/" + requestPath
	}
	return fmt.Sprintf("%s://%s%s", scheme, addr, requestPath)
}

// executeMasterRequest queries masters with candidate failover and 307 redirect tracking.
func (c *DefaultClient) executeMasterRequest(ctx context.Context, target cluster.ClusterTarget, path string) (*http.Response, error) {
	if len(target.Masters) == 0 {
		return nil, ErrNoMastersConfigured
	}

	httpClient, err := c.getHTTPClient(target)
	if err != nil {
		return nil, err
	}

	// Build candidate list prioritizing cached leader or target.ActiveLeader
	c.mu.RLock()
	cachedLeader := c.leaderCache[target.ID]
	c.mu.RUnlock()

	var candidates []string
	preferredLeader := target.ActiveLeader
	if cachedLeader != "" {
		preferredLeader = cachedLeader
	}

	if preferredLeader != "" {
		candidates = append(candidates, preferredLeader)
	}
	for _, m := range target.Masters {
		if m != preferredLeader {
			candidates = append(candidates, m)
		}
	}

	var errList []string
	for _, masterAddr := range candidates {
		fullURL := buildURL(target.TLS.Enabled, masterAddr, path)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
		if err != nil {
			errList = append(errList, fmt.Sprintf("%s: %v", masterAddr, err))
			continue
		}
		req.Header.Set("Accept", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			errList = append(errList, fmt.Sprintf("%s: %v", masterAddr, err))
			continue
		}

		// Handle HTTP 307 Temporary Redirect from follower to leader
		if resp.StatusCode == http.StatusTemporaryRedirect {
			loc := resp.Header.Get("Location")
			resp.Body.Close()
			if loc != "" {
				u, parseErr := url.Parse(loc)
				if parseErr == nil && u.Host != "" {
					c.mu.Lock()
					c.leaderCache[target.ID] = u.Host
					c.mu.Unlock()

					// Re-issue request against redirected leader
					redirReq, err := http.NewRequestWithContext(ctx, http.MethodGet, loc, nil)
					if err == nil {
						redirReq.Header.Set("Accept", "application/json")
						redirResp, redirErr := httpClient.Do(redirReq)
						if redirErr == nil && redirResp.StatusCode < 500 {
							return redirResp, nil
						}
					}
				}
			}
			continue
		}

		if resp.StatusCode >= 500 {
			resp.Body.Close()
			errList = append(errList, fmt.Sprintf("%s: HTTP %d", masterAddr, resp.StatusCode))
			continue
		}

		return resp, nil
	}

	return nil, fmt.Errorf("%w: %s", ErrAllMastersUnreachable, strings.Join(errList, "; "))
}

// GetClusterStatus queries Master GET /cluster/status.
func (c *DefaultClient) GetClusterStatus(ctx context.Context, target cluster.ClusterTarget) (*ClusterStatus, error) {
	resp, err := c.executeMasterRequest(ctx, target, "/cluster/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %d", ErrBadResponse, resp.StatusCode)
	}

	var status ClusterStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("decode /cluster/status: %w", err)
	}

	if status.Leader != "" {
		c.mu.Lock()
		c.leaderCache[target.ID] = status.Leader
		c.mu.Unlock()
	}

	return &status, nil
}

// GetDirStatus queries Master GET /dir/status.
func (c *DefaultClient) GetDirStatus(ctx context.Context, target cluster.ClusterTarget) (*DirStatusResponse, error) {
	resp, err := c.executeMasterRequest(ctx, target, "/dir/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %d", ErrBadResponse, resp.StatusCode)
	}

	var dirResp DirStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&dirResp); err != nil {
		return nil, fmt.Errorf("decode /dir/status: %w", err)
	}
	return &dirResp, nil
}

// GetVolumeStatus queries Master GET /vol/status.
func (c *DefaultClient) GetVolumeStatus(ctx context.Context, target cluster.ClusterTarget) (*VolumeStatusResponse, error) {
	resp, err := c.executeMasterRequest(ctx, target, "/vol/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %d", ErrBadResponse, resp.StatusCode)
	}

	var volResp VolumeStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&volResp); err != nil {
		return nil, fmt.Errorf("decode /vol/status: %w", err)
	}
	return &volResp, nil
}

// GetTopology returns normalized cluster topology.
func (c *DefaultClient) GetTopology(ctx context.Context, target cluster.ClusterTarget) (*Topology, error) {
	dirResp, err := c.GetDirStatus(ctx, target)
	if err != nil {
		return nil, err
	}

	return &Topology{
		MaxVolumeSlots:  dirResp.Topology.Max,
		FreeVolumeSlots: dirResp.Topology.Free,
		DataCenters:     dirResp.Topology.DataCenters,
		Layouts:         dirResp.Topology.Layouts,
		Version:         dirResp.Version,
	}, nil
}

// GetVolumes returns parsed volume information with calculated garbage ratios.
func (c *DefaultClient) GetVolumes(ctx context.Context, target cluster.ClusterTarget) ([]VolumeInfo, error) {
	volResp, err := c.GetVolumeStatus(ctx, target)
	if err != nil {
		return nil, err
	}

	volumes := make([]VolumeInfo, len(volResp.Volumes))
	for i, v := range volResp.Volumes {
		var garbageRatio float64
		if v.Size > 0 && v.DeletedByteCount > 0 {
			garbageRatio = float64(v.DeletedByteCount) / float64(v.Size)
		}

		volumes[i] = VolumeInfo{
			ID:               v.Id,
			Size:             v.Size,
			ReplicaPlacement: fmt.Sprintf("%03d", v.ReplicaPlacement),
			Collection:       v.Collection,
			Version:          v.Version,
			FileCount:        uint64(v.FileCount),
			DeleteCount:      uint64(v.DeleteCount),
			DeletedByteCount: v.DeletedByteCount,
			ReadOnly:         v.ReadOnly,
			GarbageRatio:     garbageRatio,
		}
	}
	return volumes, nil
}

// ListFilerDir queries Filer directory entries with pagination limit.
func (c *DefaultClient) ListFilerDir(ctx context.Context, target cluster.ClusterTarget, dirPath string, limit int) (*FilerDirResult, error) {
	if target.Filer == "" {
		return nil, ErrFilerNotConfigured
	}
	if limit <= 0 {
		limit = 200
	}

	httpClient, err := c.getHTTPClient(target)
	if err != nil {
		return nil, err
	}

	cleanPath := path.Clean("/" + dirPath)
	if !strings.HasSuffix(cleanPath, "/") {
		cleanPath += "/"
	}
	queryPath := fmt.Sprintf("%s?pretty=y&limit=%d", cleanPath, limit)
	fullURL := buildURL(target.TLS.Enabled, target.Filer, queryPath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("filer query %s: %w", fullURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %d", ErrBadResponse, resp.StatusCode)
	}

	var result FilerDirResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode filer dir json: %w", err)
	}
	return &result, nil
}

// StreamFile downloads a file stream from Filer, returning io.ReadCloser and Content-Type.
func (c *DefaultClient) StreamFile(ctx context.Context, target cluster.ClusterTarget, filePath string) (io.ReadCloser, string, error) {
	if target.Filer == "" {
		return nil, "", ErrFilerNotConfigured
	}

	httpClient, err := c.getHTTPClient(target)
	if err != nil {
		return nil, "", err
	}

	cleanPath := path.Clean("/" + filePath)
	fullURL := buildURL(target.TLS.Enabled, target.Filer, cleanPath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, "", err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("download stream %s: %w", fullURL, err)
	}

	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, "", ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, "", fmt.Errorf("%w: HTTP %d", ErrBadResponse, resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	return resp.Body, contentType, nil
}

// UploadFile streams file upload via multipart/form-data with zero-buffer io.Pipe.
func (c *DefaultClient) UploadFile(ctx context.Context, target cluster.ClusterTarget, dirPath string, fileName string, reader io.Reader, mimeType string) error {
	if target.Filer == "" {
		return ErrFilerNotConfigured
	}

	httpClient, err := c.getHTTPClient(target)
	if err != nil {
		return err
	}

	cleanDir := path.Clean("/" + dirPath)
	if !strings.HasSuffix(cleanDir, "/") {
		cleanDir += "/"
	}
	fullURL := buildURL(target.TLS.Enabled, target.Filer, cleanDir)

	pr, pw := io.Pipe()
	bodyWriter := multipart.NewWriter(pw)

	go func() {
		var writeErr error
		defer func() {
			if writeErr != nil {
				pw.CloseWithError(writeErr)
			} else {
				pw.Close()
			}
		}()

		part, err := bodyWriter.CreateFormFile("file", fileName)
		if err != nil {
			writeErr = err
			return
		}
		if _, err = io.Copy(part, reader); err != nil {
			writeErr = err
			return
		}
		writeErr = bodyWriter.Close()
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, pr)
	if err != nil {
		pr.Close()
		return err
	}
	req.Header.Set("Content-Type", bodyWriter.FormDataContentType())

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("upload to %s: %w", fullURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("%w: upload HTTP %d", ErrBadResponse, resp.StatusCode)
	}
	return nil
}

// DeleteEntry deletes a file or directory from the Filer.
func (c *DefaultClient) DeleteEntry(ctx context.Context, target cluster.ClusterTarget, entryPath string, isDir bool) error {
	if target.Filer == "" {
		return ErrFilerNotConfigured
	}

	httpClient, err := c.getHTTPClient(target)
	if err != nil {
		return err
	}

	cleanPath := path.Clean("/" + entryPath)
	deleteURL := cleanPath
	if isDir {
		deleteURL += "?recursive=true&ignoreRecursiveError=true"
	}
	fullURL := buildURL(target.TLS.Enabled, target.Filer, deleteURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, fullURL, nil)
	if err != nil {
		return err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete entry %s: %w", fullURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("%w: delete HTTP %d", ErrBadResponse, resp.StatusCode)
	}
	return nil
}

// CheckS3Status probes the SeaweedFS S3 gateway endpoint.
func (c *DefaultClient) CheckS3Status(ctx context.Context, target cluster.ClusterTarget) (*S3EndpointStatus, error) {
	if target.S3 == "" {
		return &S3EndpointStatus{
			Available: false,
			Endpoint:  "",
			Message:   "S3 gateway endpoint not configured",
		}, nil
	}

	httpClient, err := c.getHTTPClient(target)
	if err != nil {
		return nil, err
	}

	fullURL := buildURL(target.TLS.Enabled, target.S3, "/")
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	latency := time.Since(start).Milliseconds()

	if err != nil {
		return &S3EndpointStatus{
			Available:  false,
			Endpoint:   target.S3,
			LatencyMs:  latency,
			StatusCode: 0,
			Message:    err.Error(),
		}, nil
	}
	defer resp.Body.Close()

	// S3 daemon is alive even if unauthenticated (HTTP 403 Forbidden / 400 Bad Request)
	isAlive := resp.StatusCode < 500
	return &S3EndpointStatus{
		Available:  isAlive,
		Endpoint:   target.S3,
		StatusCode: resp.StatusCode,
		LatencyMs:  latency,
		Message:    http.StatusText(resp.StatusCode),
	}, nil
}

// GetVolumeServerStatus queries a Volume server directly at GET /status.
func (c *DefaultClient) GetVolumeServerStatus(ctx context.Context, target cluster.ClusterTarget, volumeServerAddr string) (*VolumeServerStatus, error) {
	httpClient, err := c.getHTTPClient(target)
	if err != nil {
		return nil, err
	}

	fullURL := buildURL(target.TLS.Enabled, volumeServerAddr, "/status")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("volume server probe %s: %w", fullURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %d", ErrBadResponse, resp.StatusCode)
	}

	var status VolumeServerStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("decode volume server status: %w", err)
	}
	return &status, nil
}
