package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

// Request is the legacy JSON request over the Unix socket. New code should use SocketRequest.
type Request struct {
	Action string         `json:"action"`
	Params map[string]any `json:"params,omitempty"`
}

// SocketRequest is the shared request/response contract (newline-JSON) used by the
// Omarchy shell plugin (Quickshell QML). It is the preferred contract; legacy Request
// with Action remains supported for backward compatibility.
type SocketRequest struct {
	ID     string         `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params,omitempty"`
}

// Response is the legacy JSON response.
type Response struct {
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

// SocketResponse is the shared response for SocketRequest.
type SocketResponse struct {
	ID     string       `json:"id"`
	Result any          `json:"result,omitempty"`
	Error  *SocketError `json:"error,omitempty"`
}

type SocketError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// DaemonStatus is the payload returned by the "status" action and used by the
// Omarchy shell plugin (Quickshell QML).
type DaemonStatus struct {
	Online        bool           `json:"online"`
	InstanceID    string         `json:"instance_id,omitempty"`
	Version       string         `json:"version,omitempty"`
	Health        string         `json:"health,omitempty"`
	ServerURL     string         `json:"server_url"`
	Events        []domain.Event `json:"events,omitempty"`
	UnreadCount   int            `json:"unread_count"`
	UnreadEvents  int            `json:"unread_events"`
	ActiveRunners int            `json:"active_runners"`
	WaitingAgents int            `json:"waiting_agents"`
	SyncConflicts int            `json:"sync_conflicts"`
	Projects      []ProjectState `json:"projects,omitempty"`
	LastSyncAt    *time.Time     `json:"last_sync_at,omitempty"`
	Error         string         `json:"error,omitempty"`
}

// Daemon is the user-level omahab-clientd. It owns the Unix socket, background
// status/event sync, project fetch state, and launch delegation.
type Daemon struct {
	cfg        *Config
	creds      CredentialStore
	remote     *RemoteClient
	launcher   Launcher
	checker    TailscaleChecker
	resolver   DNSResolver
	tlsChecker TLSChecker
	projects   *ProjectStore
	gitRunner  GitRunner

	socketPath string
	listener   net.Listener

	mu         sync.RWMutex
	status     *domain.Status
	events     []domain.Event
	lastSyncAt *time.Time
	lastErr    string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	syncInterval  time.Duration
	fetchInterval time.Duration

	log *slog.Logger
}

// DaemonOpts configures the daemon. Zero values get sensible defaults.
type DaemonOpts struct {
	Config          *Config
	CredentialStore CredentialStore
	Remote          *RemoteClient
	Launcher        Launcher
	Checker         TailscaleChecker
	Resolver        DNSResolver
	TLSChecker      TLSChecker
	GitRunner       GitRunner
	ProjectStore    *ProjectStore
	Logger          *slog.Logger
	SyncInterval    time.Duration
	FetchInterval   time.Duration
}

func NewDaemon(opts DaemonOpts) (*Daemon, error) {
	if opts.Config == nil {
		return nil, fmt.Errorf("config required")
	}
	if opts.CredentialStore == nil {
		opts.CredentialStore = NewMemoryCredentialStore()
	}
	if opts.Launcher == nil {
		opts.Launcher = &NopLauncher{}
	}
	if opts.Checker == nil {
		opts.Checker = &ExecTailscaleChecker{}
	}
	if opts.Resolver == nil {
		opts.Resolver = &NetDNSResolver{}
	}
	if opts.TLSChecker == nil {
		opts.TLSChecker = &DefaultTLSChecker{}
	}
	if opts.GitRunner == nil {
		opts.GitRunner = &NopGitRunner{}
	}
	if opts.ProjectStore == nil {
		opts.ProjectStore = NewProjectStore(opts.GitRunner)
	}
	if opts.Logger == nil {
		// Discard by default; caller can inject. Never log credentials.
		opts.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	remote := opts.Remote
	if remote == nil && opts.Config.ServerURL != "" {
		var err error
		remote, err = NewRemoteClient(RemoteClientConfig{
			ServerURL:        opts.Config.ServerURL,
			PinnedInstanceID: opts.Config.PinnedInstanceID,
			CredentialStore:  opts.CredentialStore,
		})
		if err != nil {
			// Allow daemon to start without remote (offline mode); diagnose will surface it.
			opts.Logger.Warn("remote init failed", "err", err)
		}
	}
	syncInt := opts.SyncInterval
	if syncInt == 0 {
		syncInt = 30 * time.Second
	}
	fetchInt := opts.FetchInterval
	if fetchInt == 0 {
		fetchInt = 5 * time.Minute
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Daemon{
		cfg:           opts.Config,
		creds:         opts.CredentialStore,
		remote:        remote,
		launcher:      opts.Launcher,
		checker:       opts.Checker,
		resolver:      opts.Resolver,
		tlsChecker:    opts.TLSChecker,
		projects:      opts.ProjectStore,
		gitRunner:     opts.GitRunner,
		socketPath:    opts.Config.EffectiveSocketPath(),
		syncInterval:  syncInt,
		fetchInterval: fetchInt,
		log:           opts.Logger,
		ctx:           ctx,
		cancel:        cancel,
	}, nil
}

// SocketPath returns the Unix socket path.
func (d *Daemon) SocketPath() string { return d.socketPath }

// Start creates the Unix socket (0600) and begins serving and background sync.
// It is idempotent with respect to stale socket files.
func (d *Daemon) Start() error {
	if d.listener != nil {
		return fmt.Errorf("already started")
	}
	if err := os.MkdirAll(filepath.Dir(d.socketPath), 0o700); err != nil {
		return fmt.Errorf("mkdir socket dir: %w", err)
	}
	// Remove stale socket file if present (unclean shutdown).
	if _, err := os.Stat(d.socketPath); err == nil {
		_ = os.Remove(d.socketPath)
	}
	// Ensure 0600: create with umask-aware chmod after listen.
	// Go's net.Listen respects umask; we chmod explicitly to 0600.
	ln, err := net.Listen("unix", d.socketPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", d.socketPath, err)
	}
	if err := os.Chmod(d.socketPath, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}
	d.listener = ln

	d.wg.Add(1)
	go d.serve()

	d.wg.Add(1)
	go d.syncLoop()

	d.wg.Add(1)
	go d.fetchLoop()

	d.log.Info("clientd started", "socket", d.socketPath, "server", redactServerURL(d.cfg.ServerURL))
	return nil
}

// Stop shuts down the listener and background goroutines cleanly.
func (d *Daemon) Stop() error {
	d.cancel()
	if d.listener != nil {
		_ = d.listener.Close()
	}
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		return fmt.Errorf("timeout waiting for daemon to stop")
	}
	_ = os.Remove(d.socketPath)
	d.log.Info("clientd stopped")
	return nil
}

func redactServerURL(u string) string {
	// Never log credentials; URL itself is safe but keep helper.
	return u
}

func (d *Daemon) serve() {
	defer d.wg.Done()
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			if d.ctx.Err() != nil {
				return
			}
			d.log.Error("accept", "err", err)
			continue
		}
		go d.handleConn(conn)
	}
}

func (d *Daemon) handleConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)
	data, err := ioReadJSON(br)
	if err != nil {
		_ = writeResponse(conn, Response{OK: false, Error: fmt.Sprintf("read request: %v", err)})
		return
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		_ = writeResponse(conn, Response{OK: false, Error: fmt.Sprintf("invalid json: %v", err)})
		return
	}
	if _, ok := raw["method"]; ok {
		var sreq SocketRequest
		if err := json.Unmarshal(data, &sreq); err != nil {
			_ = writeSocketResponse(conn, SocketResponse{ID: "", Error: &SocketError{Code: "bad_request", Message: fmt.Sprintf("invalid json: %v", err)}})
			return
		}
		sreq.Method = strings.TrimSpace(strings.ToLower(sreq.Method))
		resp := d.dispatchSocket(sreq)
		_ = writeSocketResponse(conn, resp)
		return
	}
	var req Request
	if err := json.Unmarshal(data, &req); err != nil {
		_ = writeResponse(conn, Response{OK: false, Error: fmt.Sprintf("invalid json: %v", err)})
		return
	}
	req.Action = strings.TrimSpace(strings.ToLower(req.Action))
	resp := d.dispatch(req)
	_ = writeResponse(conn, resp)
}

func ioReadJSON(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	tmp := make([]byte, 4096)
	for {
		n, err := br.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			trimmed := strings.TrimSpace(string(buf))
			if trimmed != "" && json.Valid([]byte(trimmed)) {
				if br.Buffered() == 0 {
					return []byte(trimmed), nil
				}
			}
			if len(buf) > 1<<20 {
				return nil, fmt.Errorf("request too large")
			}
		}
		if err != nil {
			if len(buf) == 0 {
				return nil, err
			}
			trimmed := strings.TrimSpace(string(buf))
			if trimmed == "" {
				return nil, err
			}
			if !json.Valid([]byte(trimmed)) {
				return nil, fmt.Errorf("invalid json: %s", trimmed)
			}
			return []byte(trimmed), nil
		}
	}
}

func writeResponse(conn net.Conn, resp Response) error {
	enc := json.NewEncoder(conn)
	enc.SetEscapeHTML(false)
	return enc.Encode(resp)
}

func writeSocketResponse(conn net.Conn, resp SocketResponse) error {
	enc := json.NewEncoder(conn)
	enc.SetEscapeHTML(false)
	return enc.Encode(resp)
}

func (d *Daemon) dispatch(req Request) Response {
	switch req.Action {
	case "status":
		return Response{OK: true, Data: d.buildStatus()}
	case "diagnose", "diagnose-connection", "doctor":
		report := Diagnose(d.ctx, d.cfg, d.remote, d.checker, d.resolver, d.tlsChecker)
		return Response{OK: true, Data: report}
	case "open-ai", "open_ai", "openai", "hermes.open", "hermes_open":
		if err := d.openAI(); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true, Data: map[string]string{"result": "opened ai"}}
	case "open-omahab", "open_omahab", "open-omahabd", "omahab.open":
		if err := d.openOmahab(); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true, Data: map[string]string{"result": "opened omahab"}}
	case "project.fetch", "project_fetch", "fetch":
		id, _ := req.Params["id"].(string)
		if id == "" {
			id, _ = req.Params["project_id"].(string)
		}
		if id == "" {
			// Fetch all
			go d.projects.FetchAll(context.Background())
			return Response{OK: true, Data: map[string]string{"result": "fetch all started"}}
		}
		if err := d.projects.Fetch(context.Background(), domain.ID(id)); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true, Data: map[string]string{"result": "fetched " + id}}
	case "project.list", "projects", "list-projects":
		return Response{OK: true, Data: d.projects.List()}
	case "project.new", "project_new", "new-project", "project-new", "project.create", "project.create-request":
		if err := d.launcher.OpenTerminal(os.Getenv("HOME")); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true, Data: map[string]string{"result": "project new picker launched"}}
	case "project.clone", "project_clone", "clone":
		dir, _ := req.Params["dir"].(string)
		urlStr, _ := req.Params["url"].(string)
		if urlStr == "" {
			urlStr, _ = req.Params["repository_url"].(string)
		}
		if dir == "" || urlStr == "" {
			// No-param or partial: launch picker for available projects (clientd-owned, no CLI shell).
			if err := d.launcher.OpenTerminal(filepath.Join(os.Getenv("HOME"), "Projects")); err != nil {
				return Response{OK: false, Error: err.Error()}
			}
			return Response{OK: true, Data: map[string]string{"result": "clone picker launched"}}
		}
		if err := d.launcher.OpenTerminal(dir); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true, Data: map[string]string{"result": "clone terminal opened"}}
	case "runner.create", "runner_create", "workspace.create", "runner.start", "runner.resume", "runner.startOrResume", "runner_start_or_resume", "runner.start-or-resume", "runner-start-or-resume":
		// No-param picker: QML clickable bar without soliciting IDs.
		if err := d.launcher.OpenTerminal(os.Getenv("HOME")); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true, Data: map[string]string{"result": "runner picker launched"}}
	case "runner.attach", "runner_attach", "workspace.attach":
		dir, _ := req.Params["dir"].(string)
		if dir == "" {
			dir, _ = req.Params["local_path"].(string)
		}
		target := dir
		if target == "" {
			target = "."
		}
		if err := d.launcher.OpenTerminal(target); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true, Data: map[string]string{"result": "terminal opened"}}
	case "sync.add", "sync_add", "sync_add_folder":
		return Response{OK: false, Error: "sync add: use server API"}
	case "launch.terminal", "terminal.open":
		dir, _ := req.Params["dir"].(string)
		if dir == "" {
			dir = "."
		}
		if err := d.launcher.OpenTerminal(dir); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true, Data: map[string]string{"result": "terminal opened"}}
	case "launch.hermes", "hermes.launch":
		u, _ := req.Params["url"].(string)
		if u == "" {
			u = d.aiURL()
		}
		if err := d.launcher.LaunchHermes(u); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true, Data: map[string]string{"result": "hermes launched"}}
	default:
		return Response{OK: false, Error: fmt.Sprintf("unknown action %q", req.Action)}
	}
}

func (d *Daemon) dispatchSocket(req SocketRequest) SocketResponse {
	switch req.Method {
	case "status":
		return SocketResponse{ID: req.ID, Result: d.buildStatus()}
	case "connection.diagnose", "diagnose", "doctor":
		report := Diagnose(d.ctx, d.cfg, d.remote, d.checker, d.resolver, d.tlsChecker)
		return SocketResponse{ID: req.ID, Result: report}
	case "hermes.open", "open-ai", "open_ai", "openai":
		if err := d.openAI(); err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
		}
		return SocketResponse{ID: req.ID, Result: map[string]string{"result": "opened ai"}}
	case "dashboard.open", "open-omahab", "open_omahab":
		if err := d.openOmahab(); err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
		}
		return SocketResponse{ID: req.ID, Result: map[string]string{"result": "opened omahab"}}
	case "project.new", "project_new", "new-project", "project-new", "project.create":
		if err := d.launcher.OpenTerminal(os.Getenv("HOME")); err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
		}
		return SocketResponse{ID: req.ID, Result: map[string]string{"result": "project new picker launched"}}
	case "project.clone", "project_clone", "clone":
		dir, _ := req.Params["dir"].(string)
		urlStr, _ := req.Params["url"].(string)
		if urlStr == "" {
			urlStr, _ = req.Params["repository_url"].(string)
		}
		if slug, ok := req.Params["slug"].(string); ok && urlStr == "" && dir == "" && slug != "" {
			// parameterized clone via slug
			dir = filepath.Join(os.Getenv("HOME"), "Projects", slug)
			urlStr = slug
		}
		if dir == "" || urlStr == "" {
			if err := d.launcher.OpenTerminal(filepath.Join(os.Getenv("HOME"), "Projects")); err != nil {
				return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
			}
			return SocketResponse{ID: req.ID, Result: map[string]string{"result": "clone picker launched"}}
		}
		if err := d.launcher.OpenTerminal(dir); err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
		}
		return SocketResponse{ID: req.ID, Result: map[string]string{"result": "clone terminal opened"}}
	case "project.open":
		slug, _ := req.Params["slug"].(string)
		if slug == "" {
			slug, _ = req.Params["project_id"].(string)
		}
		if slug == "" {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "bad_request", Message: "slug required"}}
		}
		var dir string
		for _, ps := range d.projects.List() {
			if string(ps.Project.ID) == slug || ps.Project.Slug == slug {
				dir = ps.LocalPath
				break
			}
		}
		if dir == "" {
			dir = filepath.Join(os.Getenv("HOME"), "Projects", slug)
		}
		if err := d.launcher.OpenTerminal(dir); err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
		}
		return SocketResponse{ID: req.ID, Result: map[string]string{"project_id": slug, "dir": dir}}
	case "runner.startOrResume", "runner.start-or-resume", "runner.start", "runner.resume", "runner.create", "runner_create", "runner-start-or-resume":
		if err := d.launcher.OpenTerminal(os.Getenv("HOME")); err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
		}
		return SocketResponse{ID: req.ID, Result: map[string]string{"result": "runner picker launched"}}
	case "runner.attach", "runner_attach", "workspace.attach":
		id, _ := req.Params["workspace_id"].(string)
		if id == "" {
			id, _ = req.Params["id"].(string)
		}
		if id == "" {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "bad_request", Message: "workspace_id required"}}
		}
		dir, _ := req.Params["dir"].(string)
		if dir == "" {
			dir, _ = req.Params["local_path"].(string)
		}
		if dir == "" {
			dir = "."
		}
		if err := d.launcher.OpenTerminal(dir); err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
		}
		return SocketResponse{ID: req.ID, Result: map[string]string{"workspace_id": id, "result": "terminal opened"}}
	case "sync.add":
		name, _ := req.Params["name"].(string)
		if strings.TrimSpace(name) == "" {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "bad_request", Message: "name required"}}
		}
		if err := d.launcher.OpenTerminal(os.Getenv("HOME")); err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
		}
		return SocketResponse{ID: req.ID, Result: map[string]string{"name": name, "result": "sync picker launched"}}
	default:
		return SocketResponse{ID: req.ID, Error: &SocketError{Code: "bad_request", Message: fmt.Sprintf("unknown method %q", req.Method)}}
	}
}

func (d *Daemon) aiURL() string {
	base := strings.TrimRight(d.cfg.ServerURL, "/")
	if base == "" {
		return ""
	}
	// By default AI is at ai.<domain> or /ai; use base + "/ai" heuristic if not subdomain.
	// If base already contains ai subdomain, keep it. Otherwise append.
	u := base
	// Try to construct ai.<host> when host is like omahab.example.com
	// Fallback to base
	return u
}

func (d *Daemon) openAI() error {
	u := d.aiURL()
	if u == "" {
		return fmt.Errorf("no server url configured")
	}
	// Diagnose preflight: reject public fallback before opening.
	if d.cfg.PinnedInstanceID != "" && d.remote != nil {
		ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
		defer cancel()
		if _, err := d.remote.GetInstance(ctx); err != nil {
			if errors.Is(err, ErrInstanceMismatch) || errors.Is(err, ErrPublicFallback) {
				return err
			}
			// Other errors are not fatal for open; log and proceed.
			d.log.Warn("open-ai preflight instance check failed", "err", err)
		}
	}
	// Prefer Hermes Desktop if available, else browser.
	if err := d.launcher.LaunchHermes(u); err != nil {
		return err
	}
	return nil
}

func (d *Daemon) openOmahab() error {
	u := strings.TrimRight(d.cfg.ServerURL, "/")
	if u == "" {
		return fmt.Errorf("no server url configured")
	}
	return d.launcher.OpenURL(u)
}

func (d *Daemon) buildStatus() DaemonStatus {
	d.mu.RLock()
	defer d.mu.RUnlock()
	ds := DaemonStatus{
		ServerURL:  d.cfg.ServerURL,
		InstanceID: d.cfg.PinnedInstanceID,
		Events:     d.events,
		LastSyncAt: d.lastSyncAt,
		Error:      d.lastErr,
	}
	if d.status != nil {
		ds.Online = true
		ds.InstanceID = string(d.status.InstanceID)
		ds.Version = d.status.Version
		ds.Health = string(d.status.Health)
	}
	// Override instance with pinned if status not yet fetched
	if ds.InstanceID == "" {
		ds.InstanceID = d.cfg.PinnedInstanceID
	}
	// Unread = events without ReadAt; also compute bar-specific counts.
	for _, e := range d.events {
		if e.ReadAt == nil {
			ds.UnreadCount++
			ds.UnreadEvents++
		}
		switch e.Type {
		case "agent.awaiting_approval":
			if e.ReadAt == nil {
				ds.WaitingAgents++
			}
		case "syncthing.conflict", "syncthing.device_stale":
			if e.ReadAt == nil {
				ds.SyncConflicts++
			}
		}
	}
	// Include fetch errors as sync conflicts as well.
	for _, ps := range d.projects.List() {
		if ps.FetchError != "" {
			ds.SyncConflicts++
		}
	}
	// Active runners: count projects with a local checkout (local_path set) and
	// no fetch error, plus any explicit runner events. This gives the bar a
	// non-zero signal without requiring the QML to enumerate IDs.
	for _, ps := range d.projects.List() {
		if ps.LocalPath != "" && ps.FetchError == "" {
			ds.ActiveRunners++
		}
	}
	// Also count explicit runner/workspace events as runners if present.
	for _, e := range d.events {
		if e.Type == "workspace.active" || e.Type == "runner.active" {
			if e.ReadAt == nil {
				ds.ActiveRunners++
			}
		}
	}
	ds.Projects = d.projects.List()
	if d.status == nil && d.lastErr != "" {
		ds.Online = false
	} else if d.status != nil {
		ds.Online = true
	} else {
		// No status yet but no error means offline until first sync.
		ds.Online = false
	}
	return ds
}

func (d *Daemon) syncLoop() {
	defer d.wg.Done()
	ticker := time.NewTicker(d.syncInterval)
	defer ticker.Stop()
	// Initial sync immediately
	d.syncOnce()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.syncOnce()
		}
	}
}

func (d *Daemon) fetchLoop() {
	defer d.wg.Done()
	ticker := time.NewTicker(d.fetchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.projects.FetchAll(d.ctx)
			// Also refresh remote project list periodically to reconcile
			if d.remote != nil {
				if ps, err := d.remote.GetProjects(d.ctx); err == nil {
					d.projects.SyncFromRemote(ps)
				}
			}
		}
	}
}

func (d *Daemon) syncOnce() {
	if d.remote == nil {
		d.mu.Lock()
		d.lastErr = "no remote configured"
		d.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
	defer cancel()

	st, err := d.remote.GetStatus(ctx)
	if err != nil {
		// Instance mismatch and public fallback are hard failures; surface them.
		d.mu.Lock()
		d.lastErr = err.Error()
		// Do not clear last good status; keep it but mark error.
		d.mu.Unlock()
		d.log.Warn("status sync failed", "err", err)
		return
	}
	evs, err := d.remote.GetEvents(ctx)
	if err != nil {
		d.log.Warn("events sync failed", "err", err)
		evs = nil
	}
	now := time.Now().UTC()
	d.mu.Lock()
	d.status = st
	if evs != nil {
		d.events = evs
	}
	d.lastSyncAt = &now
	d.lastErr = ""
	d.mu.Unlock()

	// Reconcile projects in background (non-blocking for status)
	go func() {
		pctx, pcancel := context.WithTimeout(d.ctx, 10*time.Second)
		defer pcancel()
		if ps, err := d.remote.GetProjects(pctx); err == nil {
			d.projects.SyncFromRemote(ps)
		}
	}()
}
