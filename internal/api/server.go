package api

import (
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strings"

	"seaweedfs-ui/internal/cluster"
	"seaweedfs-ui/internal/config"
	"seaweedfs-ui/internal/filersync"
	"seaweedfs-ui/internal/weedcli"
	"seaweedfs-ui/internal/weedhttp"
)

// Server encapsulates the REST API HTTP engine and backing subsystem dependencies.
type Server struct {
	cfg      *config.Config
	registry cluster.Registry
	cli      weedcli.Client
	http     weedhttp.Client
	sync     filersync.Inspector
	assets   fs.FS
	version  string
	handler  http.Handler
}

// NewServer constructs and wires a new Server instance.
func NewServer(
	cfg *config.Config,
	registry cluster.Registry,
	cli weedcli.Client,
	http weedhttp.Client,
	sync filersync.Inspector,
	assets fs.FS,
	version ...string,
) *Server {
	ver := "0.1.0"
	if len(version) > 0 && version[0] != "" {
		ver = version[0]
	}

	s := &Server{
		cfg:      cfg,
		registry: registry,
		cli:      cli,
		http:     http,
		sync:     sync,
		assets:   assets,
		version:  ver,
	}

	s.handler = s.routes()
	return s
}

// ServeHTTP implements http.Handler, delegating to the wrapped middleware chain.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// Handler returns the underlying http.Handler.
func (s *Server) Handler() http.Handler {
	return s.handler
}

// serveSPA handles embedded static assets and SPA fallback to index.html for client-side routing.
func (s *Server) serveSPA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	cleanPath := path.Clean(r.URL.Path)
	cleanPath = strings.TrimPrefix(cleanPath, "/")
	if cleanPath == "" || cleanPath == "." {
		cleanPath = "index.html"
	}

	// API routes MUST NEVER fallback to index.html
	if strings.HasPrefix(cleanPath, "api/") || cleanPath == "api" {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "endpoint not found", nil)
		return
	}

	if s.assets == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "static assets not configured", nil)
		return
	}

	// Try reading exact file from embedded assets
	f, err := s.assets.Open(cleanPath)
	if err == nil {
		stat, err := f.Stat()
		if err == nil && !stat.IsDir() {
			defer f.Close()
			s.serveEmbeddedFile(w, r, cleanPath, f, stat)
			return
		}
		_ = f.Close()
	}

	// If the file was not found, but had a file extension that isn't html, return 404
	ext := filepath.Ext(cleanPath)
	if ext != "" && ext != ".html" {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "file not found", nil)
		return
	}

	// Fallback to index.html for client-side SPA routes (e.g. /overview, /topology, /filer)
	indexFile, err := s.assets.Open("index.html")
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "index.html not found", nil)
		return
	}
	defer indexFile.Close()

	stat, err := indexFile.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to stat index.html", nil)
		return
	}

	s.serveEmbeddedFile(w, r, "index.html", indexFile, stat)
}

func (s *Server) serveEmbeddedFile(w http.ResponseWriter, r *http.Request, name string, file fs.File, stat fs.FileInfo) {
	ext := filepath.Ext(name)
	var contentType string
	switch ext {
	case ".js":
		contentType = "application/javascript; charset=utf-8"
	case ".css":
		contentType = "text/css; charset=utf-8"
	case ".html":
		contentType = "text/html; charset=utf-8"
	case ".json":
		contentType = "application/json; charset=utf-8"
	case ".svg":
		contentType = "image/svg+xml"
	case ".png":
		contentType = "image/png"
	default:
		contentType = mime.TypeByExtension(ext)
		if contentType == "" {
			contentType = "application/octet-stream"
		}
	}

	w.Header().Set("Content-Type", contentType)

	if name == "index.html" {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}

	if seeker, ok := file.(io.ReadSeeker); ok {
		http.ServeContent(w, r, name, stat.ModTime(), seeker)
	} else {
		_, _ = io.Copy(w, file)
	}
}
