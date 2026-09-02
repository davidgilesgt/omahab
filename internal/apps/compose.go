package apps

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/omahab/omahab/internal/domain"
)

// DefaultAppsDir matches the /srv/omahab/apps layout in DESIGN.md §4.1.
const DefaultAppsDir = "/srv/omahab/apps"

// ComposeRunner implements Runner by driving the Docker Compose CLI through
// an Invoker. Each application gets one project ("omahab-<name>") and one
// directory holding its rendered compose.yaml; deploys rewrite the file so
// the on-disk definition always matches the persisted release.
type ComposeRunner struct {
	invoker Invoker
	baseDir string
	client  *http.Client
}

// NewComposeRunner builds a Compose runner. invoker nil means ExecInvoker;
// baseDir empty means DefaultAppsDir.
func NewComposeRunner(invoker Invoker, baseDir string) *ComposeRunner {
	if invoker == nil {
		invoker = ExecInvoker{}
	}
	if baseDir == "" {
		baseDir = DefaultAppsDir
	}
	return &ComposeRunner{invoker: invoker, baseDir: baseDir, client: &http.Client{}}
}

func (r *ComposeRunner) project(app domain.Application) string { return "omahab-" + app.Name }

func (r *ComposeRunner) dir(app domain.Application) string {
	return filepath.Join(r.baseDir, app.Name)
}

func (r *ComposeRunner) ensureCompose(app domain.Application, spec DeploySpec) (string, error) {
	dir := r.dir(app)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create app directory: %w", err)
	}
	path := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(path, []byte(spec.Compose), 0o600); err != nil {
		return "", fmt.Errorf("write compose definition: %w", err)
	}
	return path, nil
}

func (r *ComposeRunner) run(ctx context.Context, app domain.Application, spec DeploySpec, verbs ...string) error {
	file, err := r.ensureCompose(app, spec)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	args := append([]string{"compose", "-p", r.project(app), "-f", file}, verbs...)
	err = r.invoker.Run(ctx, spec.Env, filepath.Dir(file), &buf, &buf, "docker", args...)
	if err != nil {
		if tail := tailOf(&buf); tail != "" {
			return fmt.Errorf("docker compose %s: %w: %s", strings.Join(verbs, " "), err, tail)
		}
		return fmt.Errorf("docker compose %s: %w", strings.Join(verbs, " "), err)
	}
	return nil
}

func (r *ComposeRunner) Deploy(ctx context.Context, app domain.Application, spec DeploySpec) error {
	// --wait blocks until containers are running (and healthy when the
	// definition carries healthchecks), so a successful Deploy means the
	// stack is observable, not merely requested.
	return r.run(ctx, app, spec, "up", "-d", "--wait", "--wait-timeout", "300", "--remove-orphans")
}

func (r *ComposeRunner) Start(ctx context.Context, app domain.Application, spec DeploySpec) error {
	return r.run(ctx, app, spec, "start")
}

func (r *ComposeRunner) Stop(ctx context.Context, app domain.Application, spec DeploySpec) error {
	return r.run(ctx, app, spec, "stop")
}

func (r *ComposeRunner) Remove(ctx context.Context, app domain.Application, spec DeploySpec) error {
	// No -v: named volumes survive teardown; data deletion is a separate,
	// explicit operation.
	return r.run(ctx, app, spec, "down", "--remove-orphans")
}

// Check observes health according to the spec's HealthCheck. HTTP checks
// probe the loopback-published port; command checks exec inside the service.
func (r *ComposeRunner) Check(ctx context.Context, app domain.Application, spec DeploySpec) (domain.Health, error) {
	hc := spec.Health
	switch hc.Kind {
	case "", CheckNone:
		return domain.HealthUnknown, nil
	case CheckHTTP:
		if hc.Port <= 0 || hc.Port > 65535 {
			return domain.HealthUnknown, fmt.Errorf("%w: http health check has no port", ErrInvalid)
		}
		return probeHTTPWith(ctx, r.client, hc)
	case CheckCommand:
		if hc.Service == "" || len(hc.Command) == 0 {
			return domain.HealthUnknown, fmt.Errorf("%w: command health check needs service and command", ErrInvalid)
		}
		file, err := r.ensureCompose(app, spec)
		if err != nil {
			return domain.HealthUnknown, err
		}
		var buf bytes.Buffer
		args := []string{"compose", "-p", r.project(app), "-f", file, "exec", "-T", hc.Service}
		args = append(args, hc.Command...)
		if err := r.invoker.Run(ctx, spec.Env, filepath.Dir(file), &buf, &buf, "docker", args...); err != nil {
			return domain.HealthUnhealthy, nil
		}
		return domain.HealthHealthy, nil
	default:
		return domain.HealthUnknown, fmt.Errorf("%w: unknown health check kind %q", ErrInvalid, hc.Kind)
	}
}

// probeHTTP performs an HTTP health probe against 127.0.0.1:<port><path>
// with the default client. Shared by every runner.
func probeHTTP(ctx context.Context, hc HealthCheck, port int) (domain.Health, error) {
	return probeHTTPWith(ctx, http.DefaultClient, hc)
}

// probeHTTPWith is probeHTTP with an injectable client.
func probeHTTPWith(ctx context.Context, client *http.Client, hc HealthCheck) (domain.Health, error) {
	if hc.Port <= 0 || hc.Port > 65535 {
		return domain.HealthUnknown, fmt.Errorf("%w: http health check has no port", ErrInvalid)
	}
	checkCtx, cancel := context.WithTimeout(ctx, hc.timeout())
	defer cancel()
	path := hc.Path
	if path == "" {
		path = "/"
	}
	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d%s", hc.Port, path), nil)
	if err != nil {
		return domain.HealthUnknown, fmt.Errorf("build health request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return domain.HealthUnhealthy, nil
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return domain.HealthHealthy, nil
	}
	return domain.HealthUnhealthy, nil
}

// ExecInvoker is the production Invoker: it runs the command as a child
// process. The secret environment projection is passed to the process
// environment only — never to arguments, logs, or persisted errors.
type ExecInvoker struct{}

func (ExecInvoker) Run(ctx context.Context, env []string, dir string, stdout, stderr io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// tailOf returns the last few hundred characters of captured command output
// for error context.
func tailOf(b *bytes.Buffer) string {
	s := strings.TrimSpace(b.String())
	if len(s) > 400 {
		s = s[len(s)-400:]
	}
	return s
}
