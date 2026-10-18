package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"seaweedfs-ui/internal/api"
	"seaweedfs-ui/internal/cluster"
	"seaweedfs-ui/internal/config"
	"seaweedfs-ui/internal/filersync"
	"seaweedfs-ui/internal/pki"
	"seaweedfs-ui/internal/weedcli"
	"seaweedfs-ui/internal/weedhttp"
	"seaweedfs-ui/web"
)

var (
	// Version is populated at build time via -ldflags="-X main.Version=..."
	Version   = "0.1.0"
	GitCommit = "dev"
	BuildDate = "unknown"
)

func main() {
	// 1. Intercept version flag before argument parsing
	for _, arg := range os.Args[1:] {
		if arg == "-version" || arg == "--version" || arg == "-v" {
			fmt.Printf("seaweedfs-ui version %s (commit: %s, built: %s)\n", Version, GitCommit, BuildDate)
			os.Exit(0)
		}
	}

	// 2. Load configuration with env overrides and CLI flags
	cfg, err := config.LoadConfigWithArgs("", os.Args[1:])
	if err != nil {
		log.Fatalf("[FATAL] Configuration load failed: %v", err)
	}

	log.Printf("[INFO] SeaweedFS Admin Web UI %s starting", Version)
	log.Printf("[INFO] Listen address: %s", cfg.ListenAddr)
	log.Printf("[INFO] Security Guardrails: allowAdminActions=%t, enableRawShell=%t", cfg.AllowAdminActions, cfg.EnableRawShell)
	log.Printf("[INFO] Timeouts: queryTimeout=%v, actionTimeout=%v", cfg.QueryTimeout, cfg.ActionTimeout)
	log.Printf("[INFO] Weed binary path: %s", cfg.WeedBinaryPath)
	log.Printf("[INFO] Static clusters: %d, K8s discovery enabled: %t", len(cfg.Clusters), cfg.Discovery.Kubernetes.Enabled)

	// 3. Ensure writable tmpfs home directory for weed shell gRPC TLS
	if os.Getenv("HOME") == "" {
		_ = os.Setenv("HOME", "/tmp/seaweedfs")
	}

	// 4. Initialize PKI security.toml if TLS is enabled
	if cfg.TLS.Enabled {
		secTomlPath, err := pki.EnsureSecurityToml(cfg.TLS)
		if err != nil {
			log.Fatalf("[FATAL] Failed to initialize PKI security.toml: %v", err)
		}
		if secTomlPath != "" {
			log.Printf("[INFO] PKI security.toml initialized at: %s", secTomlPath)
		}
	}

	// Root context for background processes
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()

	// 5. Initialize Cluster Registry
	registry := cluster.NewCompositeRegistry(cfg)
	if err := registry.Start(rootCtx); err != nil {
		log.Printf("[WARN] Cluster discovery initial sync error: %v", err)
	}
	defer registry.Stop()

	// 6. Initialize Data Plane Clients
	weedcliClient := weedcli.NewClient(cfg, nil, nil, nil, nil)
	weedhttpClient := weedhttp.NewClient(cfg.QueryTimeout)
	filersyncInspector := filersync.NewInspector(10*time.Second, 60*time.Second, cfg.QueryTimeout)

	// 7. Initialize REST API Server & Embedded SPA Handler
	apiServer := api.NewServer(cfg, registry, weedcliClient, weedhttpClient, filersyncInspector, web.Content, Version)

	// 8. Configure HTTP Server with tuned timeouts
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      cfg.ActionTimeout + 15*time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// 9. Setup OS Signal Handling (SIGINT, SIGTERM)
	shutdownSignal := make(chan os.Signal, 1)
	signal.Notify(shutdownSignal, os.Interrupt, syscall.SIGTERM)

	// 10. Start Server in background goroutine
	go func() {
		log.Printf("[INFO] Server listening on http://%s", cfg.ListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[FATAL] HTTP server encountered error: %v", err)
		}
	}()

	// 11. Wait for termination signal
	sig := <-shutdownSignal
	log.Printf("[INFO] Received signal %v; initiating graceful shutdown...", sig)

	// 12. Graceful Shutdown with 15s timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("[WARN] Graceful shutdown error: %v; forcing close", err)
		_ = httpServer.Close()
	}

	registry.Stop()
	rootCancel()
	log.Printf("[INFO] SeaweedFS UI server shutdown complete")
}
