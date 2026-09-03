package apps

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/omahab/omahab/internal/domain"
)

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
