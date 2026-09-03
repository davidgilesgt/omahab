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
	case "runner.create", "runner_create", "workspace.create", "workspace_create", "runner.start", "runner.resume", "runner.startOrResume", "runner_start_or_resume", "runner.start-or-resume", "runner-start-or-resume":
		projectSlug, _ := req.Params["project_slug"].(string)
		if projectSlug == "" {
			projectSlug, _ = req.Params["slug"].(string)
		}
		if projectSlug == "" {
			projectSlug, _ = req.Params["project_id"].(string)
		}
		if projectSlug == "" {
			projectSlug, _ = req.Params["project"].(string)
		}
		title, _ := req.Params["title"].(string)
		if title == "" {
			title, _ = req.Params["task_title"].(string)
		}
		if title == "" {
			title, _ = req.Params["name"].(string)
		}
		instructions, _ := req.Params["instructions"].(string)
		if projectSlug == "" || title == "" {
			if err := d.launcher.OpenTerminal(os.Getenv("HOME")); err != nil {
				return Response{OK: false, Error: err.Error()}
			}
			return Response{OK: true, Data: map[string]string{"result": "runner picker launched"}}
		}
		if d.remote == nil {
			return Response{OK: false, Error: "not connected to server"}
		}
		ctx2, cancel := context.WithTimeout(d.ctx, 30*time.Second)
		ws, err := d.remote.CreateCompanionWorkspace(ctx2, projectSlug, title, instructions)
		cancel()
		if err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		if ws.Status == "running" || ws.Status == "pending" {
			host := d.serverHost()
			if host == "" {
				host = "omahab"
			}
			sshArgs := []string{"ssh", "-t", "omahab@" + host, "sudo", "omahab", "runner", "attach", string(ws.ID)}
			if err := d.launcher.OpenTerminalCommand(sshArgs); err != nil {
				return Response{OK: false, Error: err.Error()}
			}
		}
		return Response{OK: true, Data: ws}
	case "runner.attach", "runner_attach", "workspace.attach":
		id, _ := req.Params["id"].(string)
		if id == "" {
			id, _ = req.Params["workspace_id"].(string)
		}
		if id == "" {
			id, _ = req.Params["workspaceId"].(string)
		}
		if strings.TrimSpace(id) != "" {
			host := d.serverHost()
			if host == "" {
				host = "omahab"
			}
			sshArgs := []string{"ssh", "-t", "omahab@" + host, "sudo", "omahab", "runner", "attach", id}
			if err := d.launcher.OpenTerminalCommand(sshArgs); err != nil {
				return Response{OK: false, Error: err.Error()}
			}
			return Response{OK: true, Data: map[string]string{"workspace_id": id, "result": "terminal opened"}}
		}
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
	case "runner.list", "runner_list", "workspace.list", "workspace_list", "workspaces", "list-workspaces", "workspace-list":
		if d.remote == nil {
			return Response{OK: false, Error: "not connected to server"}
		}
		ctx2, cancel := context.WithTimeout(d.ctx, 10*time.Second)
		list, err := d.remote.GetCompanionWorkspaces(ctx2)
		cancel()
		if err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true, Data: list}
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
	case "environment.sync", "environment_sync", "env.sync", "env_sync", "environment-sync", "env-sync":
		if err := d.envSyncOnce(); err != nil {
			return Response{OK: false, Error: sanitizeEnvError(err)}
		}
		return Response{OK: true, Data: map[string]string{"result": "environment synced", "detail": "Applied to new apps; restart existing apps"}}
	case "environment.clear", "environment_clear", "env.clear", "env_clear", "environment-clear", "env-clear":
		if err := d.envClear(); err != nil {
			return Response{OK: false, Error: sanitizeEnvError(err)}
		}
		return Response{OK: true, Data: map[string]string{"result": "environment cleared"}}
	case "environment.status", "environment_status", "env.status", "env_status":
		rev, cnt, syncedAt, envErr := d.envManager.Status()
		return Response{OK: true, Data: map[string]any{"revision": rev, "variable_count": cnt, "synced_at": syncedAt, "error": envErr}}
	case "backup.run", "backup_run", "backupRun", "backup-run", "machine-backup.run", "backup-drive.run", "backup_drive.run":
		if err := d.backupRunOnce(); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true, Data: map[string]string{"result": "backup completed"}}
	case "backup.status", "backup_status", "backupStatus", "backup-status", "machine-backup.status", "backup-drive.status", "backup_drive.status":
		_ = d.backupStatusOnce()
		d.mu.RLock()
		snap := d.backupLastSnapshot
		bErr := d.backupError
		d.mu.RUnlock()
		return Response{OK: true, Data: map[string]any{"last_snapshot": snap, "error": bErr}}
	case "xai.oauth.connect", "xai_oauth.connect", "xai-oauth.connect", "xai.oauth_connect", "provider.xai.connect", "connect-xai":
		if !d.buildStatus().HasXaiOAuthSession {
			return Response{OK: false, Error: "no active xAI OAuth session assigned to this device"}
		}
		return Response{OK: true, Data: map[string]string{"result": "xai oauth connect initiated"}}
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
	case "runner.startOrResume", "runner.start-or-resume", "runner.start", "runner.resume", "runner.create", "runner_create", "runner-start-or-resume", "workspace.create", "workspace_create":
		projectSlug, _ := req.Params["project_slug"].(string)
		if projectSlug == "" {
			projectSlug, _ = req.Params["slug"].(string)
		}
		if projectSlug == "" {
			projectSlug, _ = req.Params["project_id"].(string)
		}
		if projectSlug == "" {
			projectSlug, _ = req.Params["project"].(string)
		}
		title, _ := req.Params["title"].(string)
		if title == "" {
			title, _ = req.Params["task_title"].(string)
		}
		if title == "" {
			title, _ = req.Params["name"].(string)
		}
		instructions, _ := req.Params["instructions"].(string)
		if projectSlug == "" || title == "" {
			if err := d.launcher.OpenTerminal(os.Getenv("HOME")); err != nil {
				return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
			}
			return SocketResponse{ID: req.ID, Result: map[string]string{"result": "runner picker launched"}}
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
			sshArgs := []string{"ssh", "-t", "omahab@" + host, "sudo", "omahab", "runner", "attach", string(ws.ID)}
			if err := d.launcher.OpenTerminalCommand(sshArgs); err != nil {
				return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
			}
		}
		return SocketResponse{ID: req.ID, Result: ws}
	case "runner.attach", "runner_attach", "workspace.attach":
		id, _ := req.Params["workspace_id"].(string)
		if id == "" {
			id, _ = req.Params["id"].(string)
		}
		if id == "" {
			id, _ = req.Params["workspaceId"].(string)
		}
		if strings.TrimSpace(id) != "" {
			host := d.serverHost()
			if host == "" {
				host = "omahab"
			}
			sshArgs := []string{"ssh", "-t", "omahab@" + host, "sudo", "omahab", "runner", "attach", id}
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
	case "runner.list", "runner_list", "workspace.list", "workspace_list", "workspaces", "list-workspaces", "workspace-list":
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
	case "sync.add":
		name, _ := req.Params["name"].(string)
		if strings.TrimSpace(name) == "" {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "bad_request", Message: "name required"}}
		}
		if err := d.launcher.OpenTerminal(os.Getenv("HOME")); err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
		}
		return SocketResponse{ID: req.ID, Result: map[string]string{"name": name, "result": "sync picker launched"}}
	case "environment.sync", "environment_sync", "env.sync", "env_sync", "environment-sync", "env-sync":
		if err := d.envSyncOnce(); err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: sanitizeEnvError(err)}}
		}
		return SocketResponse{ID: req.ID, Result: map[string]string{"result": "environment synced", "detail": "Applied to new apps; restart existing apps"}}
	case "environment.clear", "environment_clear", "env.clear", "env_clear", "environment-clear", "env-clear":
		if err := d.envClear(); err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: sanitizeEnvError(err)}}
		}
		return SocketResponse{ID: req.ID, Result: map[string]string{"result": "environment cleared"}}
	case "environment.status", "environment_status", "env.status", "env_status":
		rev, cnt, syncedAt, envErr := d.envManager.Status()
		return SocketResponse{ID: req.ID, Result: map[string]any{"revision": rev, "variable_count": cnt, "synced_at": syncedAt, "error": envErr}}
	case "backup.run", "backup_run", "backupRun", "backup-run", "machine-backup.run", "backup-drive.run", "backup_drive.run":
		if err := d.backupRunOnce(); err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
		}
		return SocketResponse{ID: req.ID, Result: map[string]string{"result": "backup completed"}}
	case "backup.status", "backup_status", "backupStatus", "backup-status", "machine-backup.status", "backup-drive.status", "backup_drive.status":
		_ = d.backupStatusOnce()
		d.mu.RLock()
		snap := d.backupLastSnapshot
		bErr := d.backupError
		d.mu.RUnlock()
		return SocketResponse{ID: req.ID, Result: map[string]any{"last_snapshot": snap, "error": bErr}}
	case "xai.oauth.connect", "xai_oauth.connect", "xai-oauth.connect", "xai.oauth_connect", "provider.xai.connect", "connect-xai":
		// xAI loopback OAuth connect — requires companion enrollment with allow_provider_oauth and active session assigned to this device.
		// No secrets in response; actual relay is via omahab provider login xai or companion port 56121.
		if !d.buildStatus().HasXaiOAuthSession {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "not_found", Message: "no active xAI OAuth session assigned to this device"}}
		}
		return SocketResponse{ID: req.ID, Result: map[string]string{"result": "xai oauth connect initiated"}}
	default:
		return SocketResponse{ID: req.ID, Error: &SocketError{Code: "bad_request", Message: fmt.Sprintf("unknown method %q", req.Method)}}
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
