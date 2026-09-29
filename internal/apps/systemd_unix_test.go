package apps

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/omahab/omahab/internal/domain"
)

// The embedding-worker serves HTTP over a Unix socket; its catalog health
// check is an absolute unix:// URL. The systemd runner must turn that into a
// real probe (healthy when /health answers, unhealthy when it does not)
// instead of reporting HealthUnknown.
func TestSystemdCheckUnixSocketProbe(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "embedding.sock")
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	t.Cleanup(func() {
		_ = server.Close()
		_ = ln.Close()
	})
	go func() { _ = server.Serve(ln) }()

	invoker := &unitStateInvoker{inactive: map[string]bool{}}
	runner := NewSystemdRunner(invoker, t.TempDir(), func(string) []string {
		return []string{"omahab-embedding-worker.service"}
	})
	app := domain.Application{BundleID: "embedding-worker"}
	spec := DeploySpec{Health: HealthCheck{
		Kind: CheckHTTP,
		Path: "unix://" + sock + "/health",
	}}

	health, err := runner.Check(context.Background(), app, spec)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if health != domain.HealthHealthy {
		t.Fatalf("health = %q, want %q with a live socket", health, domain.HealthHealthy)
	}

	// Socket gone (worker stopped answering): the probe must report
	// unhealthy, not unknown.
	_ = ln.Close()
	_ = os.Remove(sock)
	health, err = runner.Check(context.Background(), app, spec)
	if err != nil {
		t.Fatalf("dead socket check: %v", err)
	}
	if health != domain.HealthUnhealthy {
		t.Fatalf("dead socket health = %q, want %q", health, domain.HealthUnhealthy)
	}
}

// Catalog validation must accept the absolute unix:// probe URL without a
// port, and still reject malformed relative paths.
func TestValidateHealthCheckAbsoluteURL(t *testing.T) {
	h, problems := validateHealthCheck(HealthCheck{
		Kind: CheckHTTP,
		Path: "unix:///run/omahab-embedding/embedding.sock/health",
	}, 0)
	if len(problems) != 0 {
		t.Fatalf("unix probe problems = %v, want none", problems)
	}
	if h.Kind != CheckHTTP {
		t.Fatalf("kind = %q, want http", h.Kind)
	}

	_, problems = validateHealthCheck(HealthCheck{Kind: CheckHTTP, Path: "not-a-path"}, 0)
	if len(problems) == 0 {
		t.Fatal("relative non-slash path must be rejected")
	}
}
