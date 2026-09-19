package apps

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"testing"

	"github.com/omahab/omahab/internal/domain"
)

type unitStateInvoker struct {
	inactive map[string]bool
	calls    [][]string
}

func (f *unitStateInvoker) Run(_ context.Context, _ []string, _ string, _, _ io.Writer, name string, args ...string) error {
	call := append([]string{name}, args...)
	f.calls = append(f.calls, call)
	if name == "systemctl" && len(args) == 3 && args[0] == "is-active" && args[1] == "--quiet" && f.inactive[args[2]] {
		return errors.New("inactive")
	}
	return nil
}

func serveHealth(t *testing.T, address string) {
	t.Helper()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("listen %s: %v", address, err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
	})
	go func() {
		_ = server.Serve(listener)
	}()
}

func woodpeckerUnits(string) []string {
	return []string{"woodpecker-server.service", "woodpecker-agent-docker.service"}
}

func TestWoodpeckerHealthRequiresAgent(t *testing.T) {
	serveHealth(t, "127.0.0.1:8000")
	invoker := &unitStateInvoker{inactive: map[string]bool{"woodpecker-agent-docker.service": true}}
	runner := NewSystemdRunner(invoker, t.TempDir(), woodpeckerUnits)

	health, err := runner.Check(context.Background(), domain.Application{BundleID: "woodpecker"}, DeploySpec{
		Health: HealthCheck{Kind: CheckHTTP, Path: "/healthz", Port: 8000},
	})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if health != domain.HealthUnhealthy {
		t.Fatalf("health = %q, want %q when agent unit is inactive", health, domain.HealthUnhealthy)
	}
	if !slices.ContainsFunc(invoker.calls, func(call []string) bool {
		return slices.Equal(call, []string{"systemctl", "is-active", "--quiet", "woodpecker-agent-docker.service"})
	}) {
		t.Fatalf("agent unit was not checked: %#v", invoker.calls)
	}
}

func TestWoodpeckerHealthProbesAgentEndpoint(t *testing.T) {
	serveHealth(t, "127.0.0.1:8000")
	invoker := &unitStateInvoker{inactive: map[string]bool{}}
	runner := NewSystemdRunner(invoker, t.TempDir(), woodpeckerUnits)

	health, err := runner.Check(context.Background(), domain.Application{BundleID: "woodpecker"}, DeploySpec{
		Health: HealthCheck{Kind: CheckHTTP, Path: "/healthz", Port: 8000},
	})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if health != domain.HealthUnhealthy {
		t.Fatalf("health = %q, want %q when agent health endpoint is unavailable", health, domain.HealthUnhealthy)
	}
}

func TestSystemdCommandCheckRequiresActiveUnits(t *testing.T) {
	app := domain.Application{BundleID: "woodpecker"}
	spec := DeploySpec{Health: HealthCheck{Kind: CheckCommand}}

	inactive := &unitStateInvoker{inactive: map[string]bool{"woodpecker-agent-docker.service": true}}
	runner := NewSystemdRunner(inactive, t.TempDir(), woodpeckerUnits)
	health, err := runner.Check(context.Background(), app, spec)
	if err != nil {
		t.Fatalf("inactive check: %v", err)
	}
	if health != domain.HealthUnhealthy {
		t.Fatalf("inactive health = %q, want %q", health, domain.HealthUnhealthy)
	}

	active := &unitStateInvoker{inactive: map[string]bool{}}
	runner = NewSystemdRunner(active, t.TempDir(), woodpeckerUnits)
	health, err = runner.Check(context.Background(), app, spec)
	if err != nil {
		t.Fatalf("active check: %v", err)
	}
	if health != domain.HealthUnknown {
		t.Fatalf("active health = %q, want unsupported command result %q", health, domain.HealthUnknown)
	}
}
