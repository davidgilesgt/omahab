package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ClientdClient talks to omahab-clientd over a Unix domain socket.
// The shell plugin (QML) also talks to this daemon; the CLI delegates
// desktop actions (hermes open, project clone/open, runner attach) there
// when available.
type ClientdClient struct {
	SocketPath string
	HTTPClient *http.Client
}

// DefaultClientdSocketPath returns the Unix socket path.
// Precedence: OMAHAB_CLIENTD_SOCKET env, $XDG_RUNTIME_DIR, /run/user/<uid>, temp fallback.
func DefaultClientdSocketPath() string {
	if p := os.Getenv("OMAHAB_CLIENTD_SOCKET"); p != "" {
		return p
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "omahab-clientd.sock")
	}
	// Try /run/user/<uid>
	uid := os.Getuid()
	candidate := filepath.Join("/run", "user", fmt.Sprint(uid), "omahab-clientd.sock")
	if _, err := os.Stat(filepath.Dir(candidate)); err == nil {
		return candidate
	}
	// Fallback to XDG config runtime dir
	home, _ := os.UserHomeDir()
	if home != "" {
		return filepath.Join(home, ".config", "omahab", "clientd.sock")
	}
	return filepath.Join(os.TempDir(), "omahab-clientd.sock")
}

// NewClientdClient creates a client bound to socketPath.
func NewClientdClient(socketPath string) *ClientdClient {
	if socketPath == "" {
		socketPath = DefaultClientdSocketPath()
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &ClientdClient{
		SocketPath: socketPath,
		HTTPClient: &http.Client{
			Transport: transport,
			Timeout:   15 * time.Second,
		},
	}
}

// Available reports whether the socket exists and is dialable.
func (c *ClientdClient) Available(ctx context.Context) bool {
	conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", c.SocketPath)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (c *ClientdClient) doJSON(ctx context.Context, method, path string, body any, out any) error {
	var reqBody []byte
	var err error
	if body != nil {
		reqBody, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	// Use http://unix as dummy host for unix transport.
	url := "http://unix" + path
	var req *http.Request
	if reqBody != nil {
		req, err = http.NewRequestWithContext(ctx, method, url, bytes.NewReader(reqBody))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
	} else {
		req, err = http.NewRequestWithContext(ctx, method, url, nil)
		if err != nil {
			return err
		}
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return parseAPIError(resp)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// HermesOpen instructs clientd to open Hermes Desktop/browser at the given URL.
func (c *ClientdClient) HermesOpen(ctx context.Context, url string) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/hermes/open", map[string]string{"url": url}, nil)
}

// ProjectClone instructs clientd to clone a project locally.
func (c *ClientdClient) ProjectClone(ctx context.Context, projectID, destDir string) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/projects/clone", map[string]string{
		"project_id": projectID,
		"dest_dir":   destDir,
	}, nil)
}

// ProjectOpen instructs clientd to open a project in the editor/terminal.
func (c *ClientdClient) ProjectOpen(ctx context.Context, projectID string) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/projects/open", map[string]string{
		"project_id": projectID,
	}, nil)
}

// RunnerAttach attaches to a workspace/runner via clientd terminal.
func (c *ClientdClient) RunnerAttach(ctx context.Context, workspaceID string) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/workspaces/attach", map[string]string{
		"workspace_id": workspaceID,
	}, nil)
}

// SyncAdd delegates Syncthing folder enrollment to clientd when possible.
func (c *ClientdClient) SyncAdd(ctx context.Context, name, serverPath string, shareWithAI bool) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/sync/add", map[string]any{
		"name":          name,
		"server_path":   serverPath,
		"share_with_ai": shareWithAI,
	}, nil)
}

// Status returns clientd health/status for diagnostics.
func (c *ClientdClient) Status(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	err := c.doJSON(ctx, http.MethodGet, "/v1/status", nil, &out)
	return out, err
}

// ProjectCreate asks clientd to create a new project (or launch picker when name empty).
func (c *ClientdClient) ProjectCreate(ctx context.Context, name, slug string) error {
	if strings.TrimSpace(name) == "" && strings.TrimSpace(slug) == "" {
		return c.doJSON(ctx, http.MethodPost, "/v1/projects/new", map[string]string{}, nil)
	}
	return c.doJSON(ctx, http.MethodPost, "/v1/projects/create", map[string]string{"name": name, "slug": slug}, nil)
}

// ProjectNew launches clientd's no-param project creation picker.
func (c *ClientdClient) ProjectNew(ctx context.Context) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/projects/new", map[string]string{}, nil)
}

// WorkspaceStartOrResume launches clientd's picker to start or resume a runner without params.
func (c *ClientdClient) WorkspaceStartOrResume(ctx context.Context) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/workspaces/startOrResume", map[string]string{}, nil)
}

// DashboardOpen asks clientd to open the dashboard URL after private-route gate.
func (c *ClientdClient) DashboardOpen(ctx context.Context) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/dashboard/open", map[string]string{}, nil)
}

// jsonReader helpers removed; bytes.NewReader used directly.
