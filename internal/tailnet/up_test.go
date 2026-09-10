package tailnet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeTailscale installs an executable `tailscale` shim on PATH whose
// behavior depends on argv[1]: "status ..." and "up ..." are handled by
// the given shell bodies.
func fakeTailscale(t *testing.T, statusBody, upBody string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"status\" ]; then\n" + statusBody + "\nfi\n" +
		"if [ \"$1\" = \"up\" ]; then\n" + upBody + "\nfi\n" +
		"exit 0\n"
	p := filepath.Join(dir, "tailscale")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

func TestUpReturnsURLWithoutWaitingForExit(t *testing.T) {
	// Not running; `up` prints the URL then hangs (login pending).
	fakeTailscale(t,
		`echo '{"BackendState":"NeedsLogin","Self":{"TailscaleIPs":[]}}'`,
		"echo 'To authenticate, visit:'\necho 'https://login.tailscale.com/a/TEST123'\nexec sleep 20")
	start := time.Now()
	url, err := Up(context.Background())
	el := time.Since(start)
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if url != "https://login.tailscale.com/a/TEST123" {
		t.Fatalf("url = %q", url)
	}
	// Must return on URL print, not on process exit (~110s) or urlWait (30s).
	if el > 25*time.Second {
		t.Fatalf("Up took %s; should return as soon as the URL prints", el)
	}
}

func TestUpAlreadyRunningSkipsCommand(t *testing.T) {
	fakeTailscale(t,
		`echo '{"BackendState":"Running","Self":{"TailscaleIPs":["100.1.2.3"]}}'`,
		"echo SHOULD-NOT-RUN >&2\nexit 1")
	url, err := Up(context.Background())
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if url != "" {
		t.Fatalf("url = %q, want empty when already running", url)
	}
}

func TestUpCleanExitNoURLMeansEnrolled(t *testing.T) {
	fakeTailscale(t,
		`echo '{"BackendState":"NeedsLogin","Self":{"TailscaleIPs":[]}}'`,
		"exit 0")
	url, err := Up(context.Background())
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if url != "" {
		t.Fatalf("url = %q, want empty on clean exit", url)
	}
}

func TestUpHardErrorSurfacesOutput(t *testing.T) {
	fakeTailscale(t,
		`echo '{"BackendState":"NeedsLogin","Self":{"TailscaleIPs":[]}}'`,
		"echo 'something failed' >&2\nexit 1")
	_, err := Up(context.Background())
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "something failed") {
		t.Fatalf("err = %q, want output included", err)
	}
}
