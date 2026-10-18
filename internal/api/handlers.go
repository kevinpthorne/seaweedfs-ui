package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"seaweedfs-ui/internal/cluster"
	"seaweedfs-ui/internal/filersync"
	"seaweedfs-ui/internal/weedcli"
	"seaweedfs-ui/internal/weedhttp"
)

// resolveCluster resolves a ClusterTarget from the URL {id} path parameter.
func (s *Server) resolveCluster(r *http.Request) (*cluster.ClusterTarget, error) {
	id := r.PathValue("id")
	if id == "" {
		return nil, errors.New("cluster id is required")
	}
	target, err := s.registry.GetCluster(r.Context(), id)
	if err != nil {
		return nil, err
	}
	return target, nil
}

// handleHealthz handles GET /api/v1/healthz
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":    "ok",
		"version":   s.version,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

// handleConfig handles GET /api/v1/config
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"allowAdminActions": s.cfg.AllowAdminActions,
		"enableRawShell":    s.cfg.EnableRawShell,
		"version":           s.version,
		"queryTimeout":      s.cfg.QueryTimeout.String(),
		"actionTimeout":     s.cfg.ActionTimeout.String(),
	})
}

// handleListClusters handles GET /api/v1/clusters
func (s *Server) handleListClusters(w http.ResponseWriter, r *http.Request) {
	clusters, err := s.registry.ListClusters(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error(), nil)
		return
	}
	if clusters == nil {
		clusters = []cluster.ClusterTarget{}
	}
	writeJSON(w, http.StatusOK, clusters)
}

// handleClusterOverview handles GET /api/v1/clusters/{id}/overview
func (s *Server) handleClusterOverview(w http.ResponseWriter, r *http.Request) {
	target, err := s.resolveCluster(r)
	if err != nil {
		if errors.Is(err, cluster.ErrClusterNotFound) || strings.Contains(err.Error(), "cluster not found") {
			writeError(w, http.StatusNotFound, "CLUSTER_NOT_FOUND", "cluster not found", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error(), nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()

	status, err := s.http.GetClusterStatus(ctx, *target)
	if err != nil {
		writeError(w, http.StatusBadGateway, "UPSTREAM_UNAVAILABLE",
			fmt.Sprintf("master unreachable: %v", err), nil)
		return
	}

	topo, _ := s.http.GetTopology(ctx, *target)
	vols, _ := s.http.GetVolumes(ctx, *target)

	var totalUsedBytes uint64
	var activeVolCount int
	for _, v := range vols {
		totalUsedBytes += v.Size
		activeVolCount++
	}

	maxSlots := 0
	freeSlots := 0
	if topo != nil {
		maxSlots = topo.MaxVolumeSlots
		freeSlots = topo.FreeVolumeSlots
	}

	overview := map[string]interface{}{
		"clusterId": target.ID,
		"leader":    status.Leader,
		"isLeader":  status.IsLeader,
		"raftPeers": status.Peers,
		"capacity": map[string]interface{}{
			"usedBytes": totalUsedBytes,
		},
		"volumes": map[string]interface{}{
			"total": activeVolCount,
			"max":   maxSlots,
			"free":  freeSlots,
		},
		"filerAvailable": target.Filer != "",
		"s3Available":    target.S3 != "",
	}

	writeJSON(w, http.StatusOK, overview)
}

// handleClusterTopology handles GET /api/v1/clusters/{id}/topology
func (s *Server) handleClusterTopology(w http.ResponseWriter, r *http.Request) {
	target, err := s.resolveCluster(r)
	if err != nil {
		if errors.Is(err, cluster.ErrClusterNotFound) || strings.Contains(err.Error(), "cluster not found") {
			writeError(w, http.StatusNotFound, "CLUSTER_NOT_FOUND", "cluster not found", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error(), nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()

	topo, err := s.http.GetTopology(ctx, *target)
	if err != nil {
		writeError(w, http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, topo)
}

// handleClusterVolumes handles GET /api/v1/clusters/{id}/volumes
func (s *Server) handleClusterVolumes(w http.ResponseWriter, r *http.Request) {
	target, err := s.resolveCluster(r)
	if err != nil {
		if errors.Is(err, cluster.ErrClusterNotFound) || strings.Contains(err.Error(), "cluster not found") {
			writeError(w, http.StatusNotFound, "CLUSTER_NOT_FOUND", "cluster not found", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error(), nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()

	vols, err := s.http.GetVolumes(ctx, *target)
	if err != nil {
		writeError(w, http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", err.Error(), nil)
		return
	}

	colFilter := r.URL.Query().Get("collection")
	roFilter := r.URL.Query().Get("readOnly")
	garbageFilter := r.URL.Query().Get("highGarbage")

	var filtered []weedhttp.VolumeInfo
	for _, v := range vols {
		if colFilter != "" && !strings.EqualFold(v.Collection, colFilter) {
			continue
		}
		if roFilter != "" {
			roBool, err := strconv.ParseBool(roFilter)
			if err == nil && v.ReadOnly != roBool {
				continue
			}
		}
		if garbageFilter == "true" && v.GarbageRatio < 0.30 {
			continue
		}
		filtered = append(filtered, v)
	}

	if filtered == nil {
		filtered = []weedhttp.VolumeInfo{}
	}
	writeJSON(w, http.StatusOK, filtered)
}

// handleClusterAction handles POST /api/v1/clusters/{id}/actions/{action}
func (s *Server) handleClusterAction(w http.ResponseWriter, r *http.Request) {
	target, err := s.resolveCluster(r)
	if err != nil {
		if errors.Is(err, cluster.ErrClusterNotFound) || strings.Contains(err.Error(), "cluster not found") {
			writeError(w, http.StatusNotFound, "CLUSTER_NOT_FOUND", "cluster not found", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error(), nil)
		return
	}

	action := r.PathValue("action")
	switch action {
	case "vacuum", "balance", "fix.replication", "volume.grow":
	default:
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
			fmt.Sprintf("unsupported action %q", action), nil)
		return
	}

	var req struct {
		Collection  string `json:"collection"`
		Replication string `json:"replication"`
		DataCenter  string `json:"dataCenter"`
		Count       int    `json:"count"`
	}
	if r.Body != nil && r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	if err := weedcli.SanitizeArg(req.Collection); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error(), nil)
		return
	}
	if err := weedcli.SanitizeReplication(req.Replication); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error(), nil)
		return
	}
	if err := weedcli.SanitizeArg(req.DataCenter); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error(), nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.ActionTimeout)
	defer cancel()

	start := time.Now()
	var res *weedcli.ShellResult
	switch action {
	case "vacuum":
		res, err = s.cli.Vacuum(ctx, *target, req.Collection)
	case "balance":
		res, err = s.cli.Balance(ctx, *target)
	case "fix.replication":
		res, err = s.cli.FixReplication(ctx, *target)
	case "volume.grow":
		count := req.Count
		if count <= 0 {
			count = 1
		}
		res, err = s.cli.Grow(ctx, *target, count, req.Collection, req.Replication, req.DataCenter)
	}

	duration := time.Since(start)

	if err != nil {
		if errors.Is(err, weedcli.ErrClusterBusy) {
			writeError(w, http.StatusConflict, "CLUSTER_BUSY", err.Error(), nil)
			return
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, weedcli.ErrActionTimeout) {
			writeError(w, http.StatusGatewayTimeout, "ACTION_TIMEOUT", "action timed out", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "EXECUTION_FAILED", err.Error(), nil)
		return
	}

	stdout := ""
	if res != nil {
		stdout = res.Stdout
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"action":     action,
		"clusterId":  target.ID,
		"status":     "completed",
		"stdout":     stdout,
		"durationMs": duration.Milliseconds(),
	})
}

// handleClusterFilerSync handles GET /api/v1/clusters/{id}/filersync
func (s *Server) handleClusterFilerSync(w http.ResponseWriter, r *http.Request) {
	target, err := s.resolveCluster(r)
	if err != nil {
		if errors.Is(err, cluster.ErrClusterNotFound) || strings.Contains(err.Error(), "cluster not found") {
			writeError(w, http.StatusNotFound, "CLUSTER_NOT_FOUND", "cluster not found", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error(), nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()

	jobs, err := s.sync.InspectSyncJobs(ctx, *target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error(), nil)
		return
	}

	res := make([]filersync.FilerSyncStatus, 0, len(jobs))
	for i, job := range jobs {
		id := fmt.Sprintf("%s-sync-%d", target.ID, i)
		name := job.SourcePath + "->" + job.TargetPath
		if job.SourceFiler != "" && job.TargetFiler != "" {
			name = job.SourceFiler + "->" + job.TargetFiler
		}
		res = append(res, job.ToFilerSyncStatus(id, name, string(target.Source)))
	}
	writeJSON(w, http.StatusOK, res)
}

// handleListS3Buckets handles GET /api/v1/clusters/{id}/s3/buckets
func (s *Server) handleListS3Buckets(w http.ResponseWriter, r *http.Request) {
	target, err := s.resolveCluster(r)
	if err != nil {
		if errors.Is(err, cluster.ErrClusterNotFound) || strings.Contains(err.Error(), "cluster not found") {
			writeError(w, http.StatusNotFound, "CLUSTER_NOT_FOUND", "cluster not found", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error(), nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()

	buckets, err := s.cli.ListBuckets(ctx, *target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "EXECUTION_FAILED", err.Error(), nil)
		return
	}

	type bucketItem struct {
		Name string `json:"name"`
	}
	items := make([]bucketItem, 0, len(buckets))
	for _, b := range buckets {
		items = append(items, bucketItem{Name: b})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"buckets": items})
}

// handleCreateS3Bucket handles POST /api/v1/clusters/{id}/s3/buckets
func (s *Server) handleCreateS3Bucket(w http.ResponseWriter, r *http.Request) {
	target, err := s.resolveCluster(r)
	if err != nil {
		if errors.Is(err, cluster.ErrClusterNotFound) || strings.Contains(err.Error(), "cluster not found") {
			writeError(w, http.StatusNotFound, "CLUSTER_NOT_FOUND", "cluster not found", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error(), nil)
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "malformed json body", nil)
		return
	}
	if err := weedcli.SanitizeBucketName(req.Name); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error(), nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.ActionTimeout)
	defer cancel()

	res, err := s.cli.CreateBucket(ctx, *target, req.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "EXECUTION_FAILED", err.Error(), nil)
		return
	}

	stdout := ""
	if res != nil {
		stdout = res.Stdout
	}
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"bucket": req.Name,
		"status": "created",
		"stdout": stdout,
	})
}

// handleDeleteS3Bucket handles DELETE /api/v1/clusters/{id}/s3/buckets/{bucket}
func (s *Server) handleDeleteS3Bucket(w http.ResponseWriter, r *http.Request) {
	target, err := s.resolveCluster(r)
	if err != nil {
		if errors.Is(err, cluster.ErrClusterNotFound) || strings.Contains(err.Error(), "cluster not found") {
			writeError(w, http.StatusNotFound, "CLUSTER_NOT_FOUND", "cluster not found", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error(), nil)
		return
	}

	bucket := r.PathValue("bucket")
	if err := weedcli.SanitizeBucketName(bucket); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error(), nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.ActionTimeout)
	defer cancel()

	res, err := s.cli.DeleteBucket(ctx, *target, bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "EXECUTION_FAILED", err.Error(), nil)
		return
	}

	stdout := ""
	if res != nil {
		stdout = res.Stdout
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"bucket": bucket,
		"status": "deleted",
		"stdout": stdout,
	})
}

// handleListS3Users handles GET /api/v1/clusters/{id}/s3/users
func (s *Server) handleListS3Users(w http.ResponseWriter, r *http.Request) {
	target, err := s.resolveCluster(r)
	if err != nil {
		if errors.Is(err, cluster.ErrClusterNotFound) || strings.Contains(err.Error(), "cluster not found") {
			writeError(w, http.StatusNotFound, "CLUSTER_NOT_FOUND", "cluster not found", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error(), nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()

	users, err := s.cli.ListUsers(ctx, *target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "EXECUTION_FAILED", err.Error(), nil)
		return
	}
	if users == nil {
		users = []weedcli.S3User{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"users": users})
}

// handleFilerBrowse handles GET /api/v1/clusters/{id}/filer
func (s *Server) handleFilerBrowse(w http.ResponseWriter, r *http.Request) {
	target, err := s.resolveCluster(r)
	if err != nil {
		if errors.Is(err, cluster.ErrClusterNotFound) || strings.Contains(err.Error(), "cluster not found") {
			writeError(w, http.StatusNotFound, "CLUSTER_NOT_FOUND", "cluster not found", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error(), nil)
		return
	}

	if target.Filer == "" {
		writeError(w, http.StatusBadRequest, "FILER_NOT_CONFIGURED",
			"cluster has no filer endpoint configured", nil)
		return
	}

	dirPath := r.URL.Query().Get("path")
	if dirPath == "" {
		dirPath = "/"
	}

	limit := 200
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()

	result, err := s.http.ListFilerDir(ctx, *target, dirPath, limit)
	if err != nil {
		writeError(w, http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", err.Error(), nil)
		return
	}

	type entryItem struct {
		Name   string `json:"name"`
		IsDir  bool   `json:"isDir"`
		Size   uint64 `json:"size"`
		Mtime  string `json:"mtime"`
		Mime   string `json:"mime"`
		Chunks int    `json:"chunks"`
	}

	entries := make([]entryItem, 0, len(result.Entries))
	for _, e := range result.Entries {
		entries = append(entries, entryItem{
			Name:   e.Name(),
			IsDir:  e.IsDir(),
			Size:   e.Size(),
			Mtime:  e.Mtime.UTC().Format(time.RFC3339),
			Mime:   e.Mime,
			Chunks: len(e.Chunks),
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"path":    result.Path,
		"entries": entries,
	})
}

// handleFilerMeta handles GET /api/v1/clusters/{id}/filer/meta
func (s *Server) handleFilerMeta(w http.ResponseWriter, r *http.Request) {
	target, err := s.resolveCluster(r)
	if err != nil {
		if errors.Is(err, cluster.ErrClusterNotFound) || strings.Contains(err.Error(), "cluster not found") {
			writeError(w, http.StatusNotFound, "CLUSTER_NOT_FOUND", "cluster not found", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error(), nil)
		return
	}

	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
			"path query parameter is required", nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()

	meta, err := s.cli.MetaCat(ctx, *target, filePath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "EXECUTION_FAILED", err.Error(), nil)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"path":     filePath,
		"metadata": meta,
	})
}

// handleShellExecute handles POST /api/v1/clusters/{id}/shell
func (s *Server) handleShellExecute(w http.ResponseWriter, r *http.Request) {
	target, err := s.resolveCluster(r)
	if err != nil {
		if errors.Is(err, cluster.ErrClusterNotFound) || strings.Contains(err.Error(), "cluster not found") {
			writeError(w, http.StatusNotFound, "CLUSTER_NOT_FOUND", "cluster not found", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error(), nil)
		return
	}

	var req struct {
		Command string `json:"command"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "malformed json body", nil)
		return
	}

	cmdStr := strings.TrimSpace(req.Command)
	if cmdStr == "" {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "command cannot be empty", nil)
		return
	}

	// Preset check
	isPreset := false
	presets := []string{
		"volume.list", "cluster.status", "s3.bucket.list", "s3.configure",
		"fs.ls", "fs.meta.cat", "ec.shard.list",
	}
	for _, p := range presets {
		if cmdStr == p || strings.HasPrefix(cmdStr, p+" ") {
			isPreset = true
			break
		}
	}

	isRaw := !isPreset
	if isRaw {
		if !s.cfg.EnableRawShell {
			writeError(w, http.StatusForbidden, "RAW_SHELL_DISABLED",
				"raw shell execution is disabled by configuration", nil)
			return
		}
		// If raw shell is enabled, mutating commands still require AllowAdminActions
		isMutating := strings.Contains(cmdStr, "delete") ||
			strings.Contains(cmdStr, "vacuum") ||
			strings.Contains(cmdStr, "balance") ||
			strings.Contains(cmdStr, "grow") ||
			strings.Contains(cmdStr, "fix.replication")
		if isMutating && !s.cfg.AllowAdminActions {
			writeError(w, http.StatusForbidden, "ADMIN_ACTIONS_DISABLED",
				"admin actions are disabled by configuration", nil)
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.ActionTimeout)
	defer cancel()

	res, err := s.cli.Execute(ctx, *target, cmdStr, isRaw)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "EXECUTION_FAILED", err.Error(), nil)
		return
	}

	writeJSON(w, http.StatusOK, res)
}
