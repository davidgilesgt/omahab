package main

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/omahab/omahab/internal/api"
	"github.com/omahab/omahab/internal/config"
	"github.com/omahab/omahab/internal/controlplane"
	"github.com/omahab/omahab/internal/store"
)

var (
	version = "dev"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "omahabd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := cfg.EnsureDirectories(); err != nil {
		return fmt.Errorf("ensure directories: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	// Open store
	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Construct backend (migrates, instance, tokens)
	backend, err := controlplane.New(ctx, st, controlplane.Options{
		Config:    cfg,
		Version:   version,
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("init backend: %w", err)
	}

	// Signal handling
	sigCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Background schedulers: workspaces idle expiry, syncthing poll
	go backend.StartIdleExpirer(sigCtx, time.Minute)
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-sigCtx.Done():
				return
			case <-ticker.C:
				if err := backend.PollSyncthing(sigCtx); err != nil {
					logger.Error("syncthing poll failed", "error", err)
				}
			}
		}
	}()
	// Create API server
	token := backend.APIToken()
	emailKey := backend.EmailHMACKey()
	mcpToken := backend.MCPToken(context.Background())
	var mcpHandler http.Handler
	if h := backend.MCPHandler(); h != nil {
		mcpHandler = h.Handler()
	}
	srv, err := api.New(api.Config{
		Backend:      backend,
		Version:      version,
		BearerToken:  token,
		MCPToken:     mcpToken,
		MCPHandler:   mcpHandler,
		EmailHMACKey: string(emailKey),
		Bootstrap:    backend,
	})
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}

	// First-boot bootstrap listener (LAN wizard, :8485). Serves the same
	// handler; only active while bootstrap-done is absent, and closed on
	// bootstrap completion.
	var bootSrv *http.Server
	if addr := strings.TrimSpace(os.Getenv("OMAHAB_BOOTSTRAP_LISTEN")); addr != "" && backend.Active() {
		bootSrv = &http.Server{
			Addr:         addr,
			Handler:      buildHandler(srv, logger),
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 60 * time.Second,
			IdleTimeout:  120 * time.Second,
		}
		backend.SetBootstrapClose(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = bootSrv.Shutdown(ctx)
			logger.Info("bootstrap listener closed")
		})
		go func() {
			logger.Info("bootstrap listener active", "addr", addr)
			if err := bootSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("bootstrap listener failed", "error", err)
			}
		}()
	}

	// Build handler with static serving + API
	handler := buildHandler(srv, logger)

	httpSrv := &http.Server{
		Addr:         cfg.Listen,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Signal handling already prepared above
	errCh := make(chan error, 1)
	go func() {
		logger.Info("omahabd listening", "addr", cfg.Listen, "version", version)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-sigCtx.Done():
		logger.Info("shutting down", "signal", sigCtx.Err())
	case err := <-errCh:
		return fmt.Errorf("server error: %w", err)
	}

	// Graceful shutdown
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// fallback to httpSrv shutdown
		_ = httpSrv.Shutdown(shutdownCtx)
	}
	// also ensure httpSrv shutdown (api.Server wraps same)
	_ = httpSrv.Shutdown(shutdownCtx)
	logger.Info("omahabd stopped")
	return nil
}

func buildHandler(apiSrv *api.Server, logger *slog.Logger) http.Handler {
	apiRouter := apiSrv.Handler()
	// Find static dir
	staticDir := findStaticDir()
	if staticDir == "" {
		logger.Info("web assets not found, serving API only")
		return apiRouter
	}
	logger.Info("serving web assets", "dir", staticDir)
	// Use file server with SPA fallback
	fileServer := http.FileServer(http.Dir(staticDir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// API and health always go to router
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/up" || strings.HasPrefix(r.URL.Path, "/up/") {
			apiRouter.ServeHTTP(w, r)
			return
		}
		// Try static file
		path := filepath.Join(staticDir, filepath.Clean(r.URL.Path))
		// prevent traversal outside staticDir
		if !strings.HasPrefix(path, staticDir) {
			apiRouter.ServeHTTP(w, r)
			return
		}
		info, err := os.Stat(path)
		if err == nil && !info.IsDir() {
			fileServer.ServeHTTP(w, r)
			return
		}
		// For directories or not found, try SPA fallback if request accepts html
		if isSPAFallback(r) {
			// serve index.html
			index := filepath.Join(staticDir, "index.html")
			if _, err := os.Stat(index); err == nil {
				http.ServeFile(w, r, index)
				return
			}
		}
		// Fallback to file server (will 404) or API 404
		// If path has extension, serve 404 via file server; otherwise SPA
		if strings.Contains(r.URL.Path, ".") {
			fileServer.ServeHTTP(w, r)
			return
		}
		// SPA fallback
		index := filepath.Join(staticDir, "index.html")
		if _, err := os.Stat(index); err == nil {
			http.ServeFile(w, r, index)
			return
		}
		apiRouter.ServeHTTP(w, r)
	})
}

func isSPAFallback(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "text/html") {
		return true
	}
	// No accept or prefers html for navigation
	if r.Method == http.MethodGet && !strings.Contains(r.URL.Path, ".") {
		return true
	}
	return false
}

func findStaticDir() string {
	candidates := []string{}
	// Explicit override wins (NixOS module sets OMAHAB_WEB_DIR).
	if v := strings.TrimSpace(os.Getenv("OMAHAB_WEB_DIR")); v != "" {
		candidates = append(candidates, v)
	}
	candidates = append(candidates,
		"web/dist",
		"../web/dist",
		filepath.Join(filepath.Dir(os.Args[0]), "..", "web", "dist"),
		filepath.Join(filepath.Dir(os.Args[0]), "web", "dist"),
		"/usr/share/omahab/web",
	)
	// Also check executable directory
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "web", "dist"))
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "..", "web", "dist"))
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			// Must contain index.html or assets
			if _, err := fs.Stat(os.DirFS(c), "index.html"); err == nil {
				abs, _ := filepath.Abs(c)
				return abs
			}
			// even without index, treat as static dir if it exists and has files
			abs, _ := filepath.Abs(c)
			// check if any file exists inside
			entries, _ := os.ReadDir(c)
			if len(entries) > 0 {
				return abs
			}
		}
	}
	return ""
}
