package api

import (
	"net/http"
)

// routes registers all 15 API endpoints and SPA fallback onto a net/http.ServeMux,
// wrapped in the global middleware chain (Recovery -> Tracing -> CORS).
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// 1. Health and Configuration
	mux.HandleFunc("GET /api/v1/healthz", s.handleHealthz)
	mux.HandleFunc("GET /api/v1/config", s.handleConfig)

	// 2. Clusters Discovery and Metadata
	mux.HandleFunc("GET /api/v1/clusters", s.handleListClusters)
	mux.HandleFunc("GET /api/v1/clusters/{id}/overview", s.handleClusterOverview)
	mux.HandleFunc("GET /api/v1/clusters/{id}/topology", s.handleClusterTopology)
	mux.HandleFunc("GET /api/v1/clusters/{id}/volumes", s.handleClusterVolumes)

	// 3. Maintenance Operations (Gated by requireAdmin)
	mux.HandleFunc("POST /api/v1/clusters/{id}/actions/{action}", s.requireAdmin(s.handleClusterAction))

	// 4. Filer Sync Monitor
	mux.HandleFunc("GET /api/v1/clusters/{id}/filersync", s.handleClusterFilerSync)

	// 5. S3 Buckets and IAM Users (Mutating actions gated by requireAdmin)
	mux.HandleFunc("GET /api/v1/clusters/{id}/s3/buckets", s.handleListS3Buckets)
	mux.HandleFunc("POST /api/v1/clusters/{id}/s3/buckets", s.requireAdmin(s.handleCreateS3Bucket))
	mux.HandleFunc("DELETE /api/v1/clusters/{id}/s3/buckets/{bucket}", s.requireAdmin(s.handleDeleteS3Bucket))
	mux.HandleFunc("GET /api/v1/clusters/{id}/s3/users", s.handleListS3Users)

	// 6. Filer Directory Browser & Metadata
	mux.HandleFunc("GET /api/v1/clusters/{id}/filer", s.handleFilerBrowse)
	mux.HandleFunc("GET /api/v1/clusters/{id}/filer/meta", s.handleFilerMeta)

	// 7. Weed Shell Console (Gated inside handler by safe presets or enableRawShell)
	mux.HandleFunc("POST /api/v1/clusters/{id}/shell", s.handleShellExecute)

	// 8. Single-Page Application & Static Asset Fallback
	mux.HandleFunc("/", s.serveSPA)

	// Global middleware chain: Recovery -> Tracing -> CORS
	return s.recoveryMiddleware(
		s.tracingMiddleware(
			s.corsMiddleware(mux),
		),
	)
}
