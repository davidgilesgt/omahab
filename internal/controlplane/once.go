package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"time"

	"github.com/omahab/omahab/internal/projects"
)

// ErrNotConfigured is returned when an external upstream (ONCE, Cloudflare, PocketID) is not configured.
var ErrNotConfigured = errors.New("not configured")

// CommandOnceRunner implements projects.ONCERunner via the omahab-once binary.
// It calls `omahab-once` JSON/status commands with loopback/external-TLS/secrets-file args and HTTP health probing.
type CommandOnceRunner struct {
	Binary     string
	ProxyBind  string // loopback bind, e.g. 127.0.0.1:8080
	HTTPClient *http.Client
}

func NewCommandOnceRunner(binary, proxyBind string) *CommandOnceRunner {
	if binary == "" {
		binary = "omahab-once"
	}
	if proxyBind == "" {
		proxyBind = "127.0.0.1:8080"
	}
	return &CommandOnceRunner{
		Binary:     binary,
		ProxyBind:  proxyBind,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	}
}

// Deploy invokes omahab-once deploy with explicit args.
func (r *CommandOnceRunner) Deploy(ctx context.Context, in projects.DeployInput) (projects.DeployResult, error) {
	if _, err := exec.LookPath(r.Binary); err != nil {
		return projects.DeployResult{}, fmt.Errorf("%w: omahab-once binary %q not found: %v", ErrNotConfigured, r.Binary, err)
	}
	// Build args: we use JSON/status subcommand pattern
	// Example: omahab-once deploy --app <slug> --image <image> --hostname <hostname> --port <port> --health-path <path> --storage <storage> --proxy-bind <bind> --tls external --secrets-file <path> --json
	args := []string{
		"deploy",
		"--app", in.App,
		"--image", in.Image,
		"--hostname", in.Hostname,
		"--port", strconv.Itoa(in.Port),
		"--health-path", in.HealthPath,
		"--storage", in.StoragePath,
		"--proxy-bind", in.ProxyBind,
		"--tls", string(in.TLS),
		"--json",
	}
	if in.SecretsFile != "" {
		args = append(args, "--secrets-file", in.SecretsFile)
	}
	// ProxyBind override if runner has default and input empty
	if in.ProxyBind == "" && r.ProxyBind != "" {
		// replace --proxy-bind value
		for i, a := range args {
			if a == "--proxy-bind" && i+1 < len(args) {
				args[i+1] = r.ProxyBind
			}
		}
	}
	cmd := exec.CommandContext(ctx, r.Binary, args...)
	var out bytes.Buffer
	var errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	// Never log secret file contents; only path is in args
	if err := cmd.Run(); err != nil {
		return projects.DeployResult{}, fmt.Errorf("omahab-once deploy failed: %w: %s", err, errBuf.String())
	}
	// Parse JSON result: expected {"version":"...","status":"ok"}
	var res struct {
		Version string `json:"version"`
		Status  string `json:"status"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		// If output is not JSON, treat as success with raw version
		return projects.DeployResult{Version: string(bytes.TrimSpace(out.Bytes()))}, nil
	}
	if res.Error != "" {
		return projects.DeployResult{}, fmt.Errorf("omahab-once deploy error: %s", res.Error)
	}
	return projects.DeployResult{Version: res.Version}, nil
}

// Health probes via HTTP loopback and optionally via binary status command.
func (r *CommandOnceRunner) Health(ctx context.Context, in projects.HealthInput) (projects.HealthResult, error) {
	// First try HTTP probing via loopback proxy
	bind := in.ProxyBind
	if bind == "" {
		bind = r.ProxyBind
	}
	url := fmt.Sprintf("http://%s%s", bind, in.Path)
	if in.Path == "" {
		url = fmt.Sprintf("http://%s/up", bind)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return projects.HealthResult{Healthy: false, Detail: err.Error()}, nil
	}
	req.Host = in.Hostname
	if in.Hostname != "" {
		req.Header.Set("Host", in.Hostname)
	}
	client := r.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return projects.HealthResult{Healthy: true}, nil
		}
		return projects.HealthResult{Healthy: false, Detail: fmt.Sprintf("health probe %s returned %d", url, resp.StatusCode)}, nil
	}
	// Fallback to omahab-once status if HTTP fails and binary exists
	if _, lookErr := exec.LookPath(r.Binary); lookErr != nil {
		return projects.HealthResult{Healthy: false, Detail: err.Error()}, nil
	}
	args := []string{"status", "--app", in.Hostname, "--proxy-bind", bind, "--json"}
	cmd := exec.CommandContext(ctx, r.Binary, args...)
	var out bytes.Buffer
	var errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return projects.HealthResult{Healthy: false, Detail: fmt.Sprintf("health check failed: %v: %s", err, errBuf.String())}, nil
	}
	var st struct {
		Healthy bool   `json:"healthy"`
		Detail  string `json:"detail"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(out.Bytes(), &st); err != nil {
		return projects.HealthResult{Healthy: false, Detail: string(out.Bytes())}, nil
	}
	return projects.HealthResult{Healthy: st.Healthy, Detail: st.Detail}, nil
}

// Undeploy removes deployment via omahab-once.
func (r *CommandOnceRunner) Undeploy(ctx context.Context, in projects.UndeployInput) error {
	if _, err := exec.LookPath(r.Binary); err != nil {
		return fmt.Errorf("%w: omahab-once binary %q not found", ErrNotConfigured, r.Binary)
	}
	args := []string{"undeploy", "--app", in.App, "--hostname", in.Hostname, "--json"}
	cmd := exec.CommandContext(ctx, r.Binary, args...)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("omahab-once undeploy failed: %w: %s", err, errBuf.String())
	}
	return nil
}

var _ projects.ONCERunner = (*CommandOnceRunner)(nil)
