package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

// SocketRequest is the shared request/response contract (newline-JSON) used by
// the Omarchy shell plugin (Quickshell QML) and the CLI. It is the only wire
// format; see companion/PROTOCOL.md for the canonical method table.
type SocketRequest struct {
	ID     string         `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params,omitempty"`
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
	// Environment sync state (values never included).
	EnvironmentRevision      int        `json:"environment_revision"`
	EnvironmentVariableCount int        `json:"environment_variable_count"`
	EnvironmentSyncedAt      *time.Time `json:"environment_synced_at,omitempty"`
	EnvironmentError         string     `json:"environment_error,omitempty"`
	// xAI subscription OAuth loopback session assigned to this device (no secrets).
	HasXaiOAuthSession bool `json:"has_xai_oauth_session,omitempty"`
	// Machine backup (per-device restic REST) — last snapshot.
	BackupLastSnapshot *time.Time `json:"backup_last_snapshot,omitempty"`
	BackupError        string     `json:"backup_error,omitempty"`
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
	envManager *EnvironmentManager

	socketPath string
	listener   net.Listener

	mu         sync.RWMutex
	status     *domain.Status
	events     []domain.Event
	lastSyncAt *time.Time
	lastErr    string
	// Machine backup cache.
	backupLastSnapshot *time.Time
	backupError        string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	syncInterval   time.Duration
	fetchInterval  time.Duration
	envInterval    time.Duration
	backupInterval time.Duration

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
	EnvInterval     time.Duration
	BackupInterval  time.Duration
	EnvManager      *EnvironmentManager // injectable for tests; if nil, one is created
	EnvFilePath     string              // override managed file path (tests)
	EnvDBus         SystemdDBus         // injectable D-Bus (tests)
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
	envInt := opts.EnvInterval
	if envInt == 0 {
		envInt = 5 * time.Minute
	}
	backupInt := opts.BackupInterval
	if backupInt == 0 {
		backupInt = 15 * time.Minute
	}
	// Environment manager: prefer injected, else create with remote/creds.
	envMgr := opts.EnvManager
	if envMgr == nil {
		envMgr = NewEnvironmentManager(EnvironmentManagerOpts{
			Remote:   remote,
			Creds:    opts.CredentialStore,
			Logger:   opts.Logger,
			FilePath: opts.EnvFilePath,
			DBus:     opts.EnvDBus,
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Daemon{
		cfg:            opts.Config,
		creds:          opts.CredentialStore,
		remote:         remote,
		launcher:       opts.Launcher,
		checker:        opts.Checker,
		resolver:       opts.Resolver,
		tlsChecker:     opts.TLSChecker,
		projects:       opts.ProjectStore,
		gitRunner:      opts.GitRunner,
		envManager:     envMgr,
		socketPath:     opts.Config.EffectiveSocketPath(),
		syncInterval:   syncInt,
		fetchInterval:  fetchInt,
		envInterval:    envInt,
		backupInterval: backupInt,
		log:            opts.Logger,
		ctx:            ctx,
		cancel:         cancel,
	}, nil
}

// SocketPath returns the Unix socket path.

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

	d.wg.Add(1)
	go d.envLoop()

	d.wg.Add(1)
	go d.backupLoop()

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
		_ = writeSocketResponse(conn, SocketResponse{Error: &SocketError{Code: "bad_request", Message: fmt.Sprintf("read request: %v", err)}})
		return
	}
	var sreq SocketRequest
	if err := json.Unmarshal(data, &sreq); err != nil {
		_ = writeSocketResponse(conn, SocketResponse{Error: &SocketError{Code: "bad_request", Message: fmt.Sprintf("invalid json: %v", err)}})
		return
	}
	sreq.Method = strings.TrimSpace(strings.ToLower(sreq.Method))
	if sreq.Method == "" {
		_ = writeSocketResponse(conn, SocketResponse{ID: sreq.ID, Error: &SocketError{Code: "unknown_method", Message: fmt.Sprintf("unknown method %q", sreq.Method)}})
		return
	}
	resp := d.dispatchSocket(sreq)
	_ = writeSocketResponse(conn, resp)
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

func writeSocketResponse(conn net.Conn, resp SocketResponse) error {
	enc := json.NewEncoder(conn)
	enc.SetEscapeHTML(false)
	return enc.Encode(resp)
}


func (d *Daemon) dispatchSocket(req SocketRequest) SocketResponse {
	switch req.Method {
	case "status":
		return SocketResponse{ID: req.ID, Result: d.buildStatus()}
	case "diagnose":
		report := Diagnose(d.ctx, d.cfg, d.remote, d.checker, d.resolver, d.tlsChecker)
		return SocketResponse{ID: req.ID, Result: report}
	case "ai.open":
		if err := d.openAI(); err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
		}
		return SocketResponse{ID: req.ID, Result: map[string]string{"result": "opened ai"}}
	case "dashboard.open":
		if err := d.openOmahab(); err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
		}
		return SocketResponse{ID: req.ID, Result: map[string]string{"result": "opened omahab"}}
	case "project.list":
		return SocketResponse{ID: req.ID, Result: d.projects.List()}
	case "project.clone":
		slug, _ := req.Params["slug"].(string)
		slug = strings.TrimSpace(slug)
		if slug == "" {
			if v, ok := req.Params["project_id"].(string); ok {
				slug = strings.TrimSpace(v)
			}
		}
		if slug == "" {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "bad_request", Message: "slug required"}}
		}
		dir, _ := req.Params["dir"].(string)
		dir = strings.TrimSpace(dir)
		if dir == "" {
			home, _ := os.UserHomeDir()
			if home == "" {
				home = os.Getenv("HOME")
			}
			dir = filepath.Join(home, "Projects", slug)
		}
		if d.remote == nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: "not connected to server"}}
		}
		ctx2, cancel := context.WithTimeout(d.ctx, 30*time.Second)
		projects, err := d.remote.GetCompanionProjects(ctx2)
		cancel()
		if err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
		}
		var proj *domain.Project
		for i := range projects {
			if projects[i].Slug == slug || string(projects[i].ID) == slug {
				proj = &projects[i]
				break
			}
		}
		if proj == nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "not_found", Message: fmt.Sprintf("project %q not found", slug)}}
		}
		cloneURL := strings.TrimSpace(proj.RepositoryURL)
		if cloneURL == "" {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: "project has no repository_url"}}
		}
		if _, err := os.Stat(dir); err == nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "conflict", Message: fmt.Sprintf("destination %q already exists", dir)}}
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
		}
		ctx3, cancel3 := context.WithTimeout(d.ctx, 2*time.Minute)
		cmd := exec.CommandContext(ctx3, "git", "clone", cloneURL, dir)
		out, err := cmd.CombinedOutput()
		cancel3()
		if err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: fmt.Sprintf("git clone failed: %v: %s", err, string(out))}}
		}
		if d.projects != nil {
			d.projects.Upsert(*proj, dir)
		}
		if err := d.launcher.OpenTerminal(dir); err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
		}
		return SocketResponse{ID: req.ID, Result: map[string]string{"project_id": string(proj.ID), "slug": slug, "dir": dir, "clone_url": cloneURL}}
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
			home, _ := os.UserHomeDir()
			if home == "" {
				home = os.Getenv("HOME")
			}
			dir = filepath.Join(home, "Projects", slug)
		}
		if err := d.launcher.OpenTerminal(dir); err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
		}
		return SocketResponse{ID: req.ID, Result: map[string]string{"project_id": slug, "dir": dir}}
	case "workspace.list":
		if d.remote == nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: "not connected to server"}}
		}
		ctx2, cancel := context.WithTimeout(d.ctx, 10*time.Second)
		list, err := d.remote.GetCompanionWorkspaces(ctx2)
		cancel()
		if err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
		}
		return SocketResponse{ID: req.ID, Result: list}
	case "workspace.create":
		projectSlug, _ := req.Params["project_slug"].(string)
		if projectSlug == "" {
			projectSlug, _ = req.Params["slug"].(string)
		}
		title, _ := req.Params["title"].(string)
		instructions, _ := req.Params["instructions"].(string)
		if projectSlug == "" || title == "" {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "bad_request", Message: "project_slug and title required"}}
		}
		if d.remote == nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: "not connected to server"}}
		}
		ctx2, cancel := context.WithTimeout(d.ctx, 30*time.Second)
		ws, err := d.remote.CreateCompanionWorkspace(ctx2, projectSlug, title, instructions)
		cancel()
		if err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
		}
		if ws.Status == "running" || ws.Status == "pending" {
			host := d.serverHost()
			if host == "" {
				host = "omahab"
			}
			sshArgs := []string{"ssh", "-t", "omahab@" + host, "sudo", "omahab", "workspace", "attach", string(ws.ID)}
			if err := d.launcher.OpenTerminalCommand(sshArgs); err != nil {
				return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
			}
		}
		return SocketResponse{ID: req.ID, Result: ws}
	case "workspace.attach":
		id, _ := req.Params["id"].(string)
		if id == "" {
			id, _ = req.Params["workspace_id"].(string)
		}
		if strings.TrimSpace(id) != "" {
			host := d.serverHost()
			if host == "" {
				host = "omahab"
			}
			sshArgs := []string{"ssh", "-t", "omahab@" + host, "sudo", "omahab", "workspace", "attach", id}
			if err := d.launcher.OpenTerminalCommand(sshArgs); err != nil {
				return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
			}
			return SocketResponse{ID: req.ID, Result: map[string]string{"workspace_id": id, "result": "terminal opened"}}
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
	case "workspace.stop":
		id, _ := req.Params["id"].(string)
		if strings.TrimSpace(id) == "" {
			id, _ = req.Params["workspace_id"].(string)
		}
		id = strings.TrimSpace(id)
		if id == "" {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "bad_request", Message: "id required"}}
		}
		if d.remote == nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: "not connected to server"}}
		}
		ctx2, cancel := context.WithTimeout(d.ctx, 15*time.Second)
		err := d.remote.StopCompanionWorkspace(ctx2, id)
		cancel()
		if err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
		}
		return SocketResponse{ID: req.ID, Result: map[string]string{"workspace_id": id, "result": "stopped"}}
	case "environment.sync":
		if err := d.envSyncOnce(); err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: sanitizeEnvError(err)}}
		}
		return SocketResponse{ID: req.ID, Result: map[string]string{"result": "environment synced", "detail": "Applied to new apps; restart existing apps"}}
	case "environment.clear":
		if err := d.envClear(); err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: sanitizeEnvError(err)}}
		}
		return SocketResponse{ID: req.ID, Result: map[string]string{"result": "environment cleared"}}
	case "environment.status":
		rev, cnt, syncedAt, envErr := d.envManager.Status()
		return SocketResponse{ID: req.ID, Result: map[string]any{"revision": rev, "variable_count": cnt, "synced_at": syncedAt, "error": envErr}}
	case "backup.run":
		if err := d.backupRunOnce(); err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
		}
		return SocketResponse{ID: req.ID, Result: map[string]string{"result": "backup completed"}}
	case "backup.status":
		_ = d.backupStatusOnce()
		d.mu.RLock()
		snap := d.backupLastSnapshot
		bErr := d.backupError
		d.mu.RUnlock()
		return SocketResponse{ID: req.ID, Result: map[string]any{"last_snapshot": snap, "error": bErr}}
	case "subscribe":
		return SocketResponse{ID: req.ID, Result: map[string]string{"result": "subscribed"}}
	default:
		return SocketResponse{ID: req.ID, Error: &SocketError{Code: "unknown_method", Message: fmt.Sprintf("unknown method %q", req.Method)}}
	}
}

func (d *Daemon) aiURL() string {
	base := strings.TrimRight(d.cfg.ServerURL, "/")
	if base == "" {
		return ""
	}
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

func (d *Daemon) serverHost() string {
	raw := strings.TrimSpace(d.cfg.ServerURL)
	if raw == "" && d.remote != nil {
		raw = d.remote.BaseURL()
	}
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		h := raw
		if strings.Contains(h, "://") {
			h = strings.TrimPrefix(h, "http://")
			h = strings.TrimPrefix(h, "https://")
		}
		if idx := strings.Index(h, "/"); idx != -1 {
			h = h[:idx]
		}
		if idx := strings.Index(h, ":"); idx != -1 {
			h = h[:idx]
		}
		return strings.TrimSpace(h)
	}
	host := u.Host
	if strings.Contains(host, ":") {
		h, _, _ := strings.Cut(host, ":")
		host = h
	}
	return host
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
	// Environment sync state (values never included).
	if d.envManager != nil {
		rev, cnt, syncedAt, envErr := d.envManager.Status()
		ds.EnvironmentRevision = rev
		ds.EnvironmentVariableCount = cnt
		ds.EnvironmentSyncedAt = syncedAt
		ds.EnvironmentError = envErr
	}
	// xAI loopback OAuth session assigned to this device — surface only assignment, never secrets.
	// Detect via unread events of type provider_oauth pending for xAI; explicit status field when server exposes.
	for _, e := range d.events {
		if e.ReadAt != nil {
			continue
		}
		t := string(e.Type)
		if t == "provider_oauth.xai_pending" || t == "provider.oauth.xai_pending" || t == "xai.oauth.pending" {
			ds.HasXaiOAuthSession = true
			break
		}
		if len(t) >= 3 && (t == "xai_oauth" || t == "xai.oauth") {
			ds.HasXaiOAuthSession = true
			break
		}
	}
	// Machine backup cache.
	ds.BackupLastSnapshot = d.backupLastSnapshot
	ds.BackupError = d.backupError
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

func (d *Daemon) envLoop() {
	defer d.wg.Done()
	// Initial sync immediately (startup).
	_ = d.envSyncOnce()
	ticker := time.NewTicker(d.envInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			_ = d.envSyncOnce()
		}
	}
}

func (d *Daemon) envSyncOnce() error {
	if d.envManager == nil {
		return fmt.Errorf("environment manager not configured")
	}
	ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
	defer cancel()
	err := d.envManager.Sync(ctx)
	if err != nil {
		// Error already recorded in manager's status with redacted message.
		d.log.Warn("environment sync failed", "err", sanitizeEnvError(err))
		return err
	}
	return nil
}

func (d *Daemon) envClear() error {
	if d.envManager == nil {
		return fmt.Errorf("environment manager not configured")
	}
	ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
	defer cancel()
	if err := d.envManager.Clear(ctx); err != nil {
		d.log.Warn("environment clear failed", "err", sanitizeEnvError(err))
		return err
	}
	return nil
}

func (d *Daemon) backupLoop() {
	defer d.wg.Done()
	_ = d.backupStatusOnce()
	ticker := time.NewTicker(d.backupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			_ = d.backupStatusOnce()
		}
	}
}

func (d *Daemon) backupStatusOnce() error {
	ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
	defer cancel()
	st, err := StatusBackupDrive(ctx, d.creds)
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		// If no repo configured, clear error to avoid noise? But surface as backupError
		if st != nil && st.Error != "" {
			d.backupError = st.Error
		} else {
			d.backupError = err.Error()
		}
		// Keep last snapshot as is (maybe nil)
		return err
	}
	// Success — update cache, clear error.
	d.backupLastSnapshot = st.LastSnapshotTime
	d.backupError = st.Error
	return nil
}

func (d *Daemon) backupRunOnce() error {
	ctx, cancel := context.WithTimeout(d.ctx, 10*time.Minute)
	defer cancel()
	if err := RunBackupDrive(ctx, d.creds); err != nil {
		d.mu.Lock()
		d.backupError = err.Error()
		d.mu.Unlock()
		return err
	}
	// Refresh status after successful backup.
	_ = d.backupStatusOnce()
	return nil
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
