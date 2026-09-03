package client

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/syncer"
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

	subsMu sync.Mutex
	subs   map[net.Conn]struct{}

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
		subs:           make(map[net.Conn]struct{}),
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
	go d.eventLoop()

	d.wg.Add(1)
	go d.backupLoop()

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.StartHeartbeatLoop(d.ctx)
	}()

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
	// For regular request/response, 10s deadline; for subscribe we remove it.
	br := bufio.NewReader(conn)
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	data, err := ioReadJSON(br)
	if err != nil {
		_ = writeSocketResponse(conn, SocketResponse{Error: &SocketError{Code: "bad_request", Message: fmt.Sprintf("read request: %v", err)}})
		_ = conn.Close()
		return
	}
	var sreq SocketRequest
	if err := json.Unmarshal(data, &sreq); err != nil {
		_ = writeSocketResponse(conn, SocketResponse{Error: &SocketError{Code: "bad_request", Message: fmt.Sprintf("invalid json: %v", err)}})
		_ = conn.Close()
		return
	}
	sreq.Method = strings.TrimSpace(strings.ToLower(sreq.Method))
	if sreq.Method == "" {
		_ = writeSocketResponse(conn, SocketResponse{ID: sreq.ID, Error: &SocketError{Code: "unknown_method", Message: fmt.Sprintf("unknown method %q", sreq.Method)}})
		_ = conn.Close()
		return
	}
	if sreq.Method == "subscribe" {
		d.handleSubscribe(conn, sreq, br)
		return
	}
	resp := d.dispatchSocket(sreq)
	_ = writeSocketResponse(conn, resp)
	_ = conn.Close()
}

func (d *Daemon) handleSubscribe(conn net.Conn, req SocketRequest, br *bufio.Reader) {
	// No deadline for subscribed connection.
	_ = conn.SetDeadline(time.Time{})
	d.subsMu.Lock()
	d.subs[conn] = struct{}{}
	d.subsMu.Unlock()
	defer func() {
		d.subsMu.Lock()
		delete(d.subs, conn)
		d.subsMu.Unlock()
		_ = conn.Close()
	}()
	_ = writeSocketResponse(conn, SocketResponse{ID: req.ID, Result: map[string]string{"result": "subscribed"}})
	if err := writeStatusEvent(conn, d.buildStatus()); err != nil {
		return
	}
	// Wait for client close or daemon cancel. Read in goroutine to detect close.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, err := ioReadJSON(br)
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-d.ctx.Done():
	case <-done:
	}
}

func writeStatusEvent(conn net.Conn, status DaemonStatus) error {
	msg := map[string]any{"event": "status", "data": status}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	enc := json.NewEncoder(conn)
	enc.SetEscapeHTML(false)
	err := enc.Encode(msg)
	_ = conn.SetWriteDeadline(time.Time{})
	return err
}

func (d *Daemon) broadcastStatus() {
	status := d.buildStatus()
	d.subsMu.Lock()
	defer d.subsMu.Unlock()
	for conn := range d.subs {
		if err := writeStatusEvent(conn, status); err != nil {
			_ = conn.Close()
			delete(d.subs, conn)
		}
	}
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
	case "sync.add":
		name, _ := req.Params["name"].(string)
		name = strings.TrimSpace(name)
		localPath, _ := req.Params["local_path"].(string)
		localPath = strings.TrimSpace(localPath)
		if localPath == "" {
			if v, ok := req.Params["path"].(string); ok {
				localPath = strings.TrimSpace(v)
			}
		}
		if name == "" {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "bad_request", Message: "name required"}}
		}
		if localPath == "" {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "bad_request", Message: "local_path required"}}
		}
		// Expand ~ for local path
		if strings.HasPrefix(localPath, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				localPath = filepath.Join(home, localPath[2:])
			}
		} else if localPath == "~" {
			if home, err := os.UserHomeDir(); err == nil {
				localPath = home
			}
		}
		shareWithAI := false
		if v, ok := req.Params["share_with_ai"]; ok {
			switch vv := v.(type) {
			case bool:
				shareWithAI = vv
			case string:
				shareWithAI = strings.ToLower(strings.TrimSpace(vv)) == "true" || vv == "1"
			case float64:
				shareWithAI = vv != 0
			}
		}
		if d.remote == nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: "not connected to server"}}
		}
		// Read local Syncthing device ID + API key
		apiKey := syncthingAPIKey()
		deviceID, err := syncthingDeviceID(apiKey)
		if err != nil || strings.TrimSpace(deviceID) == "" {
			// If Syncthing not running, still try server creation with empty device ID? Better fail with internal.
			// Attempt to provide helpful error.
			msg := "syncthing not available locally (is Syncthing running at 127.0.0.1:8384?)"
			if err != nil {
				msg = fmt.Sprintf("syncthing device ID: %v", err)
			}
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: msg}}
		}
		hostname, _ := os.Hostname()
		hostname = strings.TrimSpace(hostname)
		if hostname == "" {
			hostname = "device"
		}
		ctx2, cancel := context.WithTimeout(d.ctx, 15*time.Second)
		folder, err := d.remote.CreateCompanionSyncFolder(ctx2, name, localPath, shareWithAI, deviceID, hostname)
		cancel()
		if err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
		}
		// Best-effort local Syncthing folder add with DefaultNotesExclusions
		_ = ensureLocalSyncthingFolder(apiKey, name, localPath, deviceID)
		return SocketResponse{ID: req.ID, Result: folder}
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
		// Ensure SSH config for editor/ssh-proxy path (best-effort)
		_ = ensureWorkspaceSSHConfig(d.serverHostOrDefault(), string(ws.ID))
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
			_ = ensureWorkspaceSSHConfig(d.serverHostOrDefault(), strings.TrimSpace(id))
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
		// Remove SSH config on successful stop (best-effort)
		_ = removeWorkspaceSSHConfig(id)
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
	case "app.open":
		app, _ := req.Params["app"].(string)
		app = strings.TrimSpace(strings.ToLower(app))
		if app == "" {
			app, _ = req.Params["name"].(string)
			app = strings.TrimSpace(strings.ToLower(app))
		}
		if app == "" {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "bad_request", Message: "app required"}}
		}
		target := appURL(d.cfg.ServerURL, app)
		if target == "" {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: "cannot derive app url"}}
		}
		// Theme is only for dashboard, but preserve behavior for home as dashboard variant
		if app == "home" || app == "dashboard" {
			target = DashboardURLWithTheme(target)
		}
		if err := d.launcher.OpenURL(target); err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
		}
		return SocketResponse{ID: req.ID, Result: map[string]string{"app": app, "url": target}}
	case "workspace.openInEditor":
		id, _ := req.Params["id"].(string)
		if strings.TrimSpace(id) == "" {
			id, _ = req.Params["workspace_id"].(string)
		}
		id = strings.TrimSpace(id)
		if id == "" {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "bad_request", Message: "id required"}}
		}
		if err := d.openWorkspaceInEditor(id); err != nil {
			return SocketResponse{ID: req.ID, Error: &SocketError{Code: "internal", Message: err.Error()}}
		}
		return SocketResponse{ID: req.ID, Result: map[string]string{"workspace_id": id, "result": "editor opened"}}
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
	u = DashboardURLWithTheme(u)
	return d.launcher.OpenURL(u)
}

func (d *Daemon) openWorkspaceInEditor(id string) error {
	host := d.serverHost()
	if host == "" {
		host = "omahab"
	}
	// Ensure SSH config entry exists for ws-<id> (best-effort)
	_ = ensureWorkspaceSSHConfig(host, id)
	// EDITOR_REMOTE is the primary knob per spec; fallback to OMAHAB_EDITOR then EDITOR for compat.
	editor := strings.TrimSpace(os.Getenv("EDITOR_REMOTE"))
	if editor == "" {
		editor = strings.TrimSpace(os.Getenv("OMAHAB_EDITOR"))
	}
	if editor == "" {
		editor = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	// Detect preferred editor command structure
	if editor == "" || strings.Contains(strings.ToLower(editor), "code") {
		// VS Code remote: code --remote ssh-remote+ws-<id> /workspace
		args := []string{"code", "--remote", "ssh-remote+ws-" + id, "/workspace"}
		if editor != "" && !strings.Contains(strings.ToLower(editor), "code") {
			// User set EDITOR to something else but we default to code
		}
		if err := d.launcher.OpenTerminalCommand(args); err == nil {
			return nil
		}
		// Fallback to ssh attach if code not available
	}
	if editor != "" && !strings.Contains(strings.ToLower(editor), "code") {
		// Try generic editor with ssh config host
		// e.g., zed ssh://ws-<id>/workspace or editor-specific
		lower := strings.ToLower(editor)
		if strings.Contains(lower, "zed") {
			if err := d.launcher.OpenTerminalCommand([]string{"zed", "ssh://ws-" + id + "/workspace"}); err == nil {
				return nil
			}
		} else {
			// Generic: try to run editor with remote path
			parts := strings.Fields(editor)
			if len(parts) > 0 {
				args := append(parts, "ssh://ws-"+id+"/workspace")
				if err := d.launcher.OpenTerminalCommand(args); err == nil {
					return nil
				}
			}
		}
	}
	// Final fallback: open tmux attach like workspace.attach
	sshArgs := []string{"ssh", "-t", "omahab@" + host, "sudo", "omahab", "workspace", "attach", id}
	return d.launcher.OpenTerminalCommand(sshArgs)
}

func ensureWorkspaceSSHConfig(host, id string) error {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
		if home == "" {
			return fmt.Errorf("no home dir")
		}
	}
	dir := filepath.Join(home, ".ssh", "config.d")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, fmt.Sprintf("omahab-ws-%s", id))
	// Include main config if not already included
	mainConfig := filepath.Join(home, ".ssh", "config")
	if data, err := os.ReadFile(mainConfig); err == nil {
		if !strings.Contains(string(data), "config.d/omahab-ws-") && !strings.Contains(string(data), "Include config.d/*") {
			// Best-effort add Include line
			f, err := os.OpenFile(mainConfig, os.O_APPEND|os.O_WRONLY, 0o600)
			if err == nil {
				_, _ = f.WriteString("\nInclude config.d/omahab-ws-*\n")
				_ = f.Close()
			}
		}
	}
	content := fmt.Sprintf("Host ws-%s\n  HostName %s\n  User omahab\n  ProxyCommand ssh omahab@%s sudo omahab workspace ssh-proxy %s\n  StrictHostKeyChecking no\n  UserKnownHostsFile /dev/null\n", id, host, host, id)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func removeWorkspaceSSHConfig(id string) error {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
		if home == "" {
			return fmt.Errorf("no home dir")
		}
	}
	path := filepath.Join(home, ".ssh", "config.d", fmt.Sprintf("omahab-ws-%s", id))
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (d *Daemon) serverHostOrDefault() string {
	h := d.serverHost()
	if strings.TrimSpace(h) == "" {
		return "omahab"
	}
	return h
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

// syncthingAPIKey reads the Syncthing API key from ~/.config/syncthing/config.xml
// It looks for apikey="..." attribute or <apikey> element.
func syncthingAPIKey() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	candidates := []string{}
	if home != "" {
		candidates = append(candidates, filepath.Join(home, ".config", "syncthing", "config.xml"))
		candidates = append(candidates, filepath.Join(home, ".local", "state", "syncthing", "config.xml"))
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); strings.TrimSpace(dir) != "" {
		candidates = append([]string{filepath.Join(strings.TrimSpace(dir), "syncthing", "config.xml")}, candidates...)
	}
	// Also try XDG_STATE_HOME
	if dir := os.Getenv("XDG_STATE_HOME"); strings.TrimSpace(dir) != "" {
		candidates = append(candidates, filepath.Join(strings.TrimSpace(dir), "syncthing", "config.xml"))
	}
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		content := string(data)
		// Search for apikey="..."
		if idx := strings.Index(content, "apikey=\""); idx != -1 {
			start := idx + len("apikey=\"")
			if end := strings.Index(content[start:], "\""); end != -1 {
				key := strings.TrimSpace(content[start : start+end])
				if key != "" {
					return key
				}
			}
		}
		// Fallback: <apikey>...</apikey>
		if start := strings.Index(content, "<apikey>"); start != -1 {
			start += len("<apikey>")
			if end := strings.Index(content[start:], "</apikey>"); end != -1 {
				key := strings.TrimSpace(content[start : start+end])
				if key != "" {
					return key
				}
			}
		}
		// XML parse fallback for robustness
		type gui struct {
			APIKey string `xml:"apikey,attr"`
		}
		type cfg struct {
			GUI gui `xml:"gui"`
		}
		var c cfg
		if err := xml.Unmarshal(data, &c); err == nil && strings.TrimSpace(c.GUI.APIKey) != "" {
			return strings.TrimSpace(c.GUI.APIKey)
		}
	}
	return ""
}

// syncthingDeviceID fetches the local Syncthing device ID via REST.
func syncthingDeviceID(apiKey string) (string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest("GET", "http://127.0.0.1:8384/rest/system/status", nil)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("syncthing status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		MyID string `json:"myID"`
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.MyID) == "" {
		return "", fmt.Errorf("syncthing myID empty")
	}
	return strings.TrimSpace(out.MyID), nil
}

// ensureLocalSyncthingFolder creates the local folder directory, writes .stignore with DefaultNotesExclusions,
// and best-effort adds the folder to local Syncthing via REST.
func ensureLocalSyncthingFolder(apiKey, folderName, localPath, deviceID string) error {
	if strings.TrimSpace(localPath) == "" || strings.TrimSpace(folderName) == "" {
		return fmt.Errorf("folder name and path required")
	}
	// Ensure directory exists
	if err := os.MkdirAll(localPath, 0755); err != nil {
		return err
	}
	// Write .stignore with DefaultNotesExclusions
	ignorePath := filepath.Join(localPath, ".stignore")
	exclusions := syncer.DefaultNotesExclusions()
	// Also add basic exclusions for safety
	content := strings.Join(exclusions, "\n") + "\n"
	_ = os.WriteFile(ignorePath, []byte(content), 0644)
	// Best-effort Syncthing REST: try to create folder via /rest/config/folders
	// If Syncthing not running, ignore error (folder will be created when Syncthing starts, or manual).
	client := &http.Client{Timeout: 5 * time.Second}
	// Try to POST folder config; Syncthing API is GET /rest/config then PUT, but we try simple POST first.
	// Build folder config payload
	folderID := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(folderName), " ", "-"))
	// Sanitize folder ID: alphanumeric + - _
	safeID := ""
	for _, r := range folderID {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			safeID += string(r)
		} else if r >= 'A' && r <= 'Z' {
			safeID += strings.ToLower(string(r))
		} else {
			safeID += "-"
		}
	}
	if safeID == "" {
		safeID = folderName
	}
	payload := map[string]any{
		"id":    safeID,
		"label": folderName,
		"path":  localPath,
		"type":  "sendreceive",
		"devices": []map[string]string{
			{"deviceID": deviceID},
		},
		"filesystemType": "basic",
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", "http://127.0.0.1:8384/rest/config/folders", strings.NewReader(string(b)))
	if err != nil {
		return nil // best-effort
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		// If POST not supported, try GET+PUT config approach
		// Fetch current config
		getReq, _ := http.NewRequest("GET", "http://127.0.0.1:8384/rest/config", nil)
		if strings.TrimSpace(apiKey) != "" {
			getReq.Header.Set("X-API-Key", apiKey)
		}
		getResp, err := client.Do(getReq)
		if err != nil || getResp.StatusCode != 200 {
			_ = body
			return nil
		}
		cfgData, _ := io.ReadAll(getResp.Body)
		_ = getResp.Body.Close()
		var cfg map[string]any
		if err := json.Unmarshal(cfgData, &cfg); err != nil {
			return nil
		}
		// Insert folder if not already present
		folders, _ := cfg["folders"].([]any)
		for _, f := range folders {
			if m, ok := f.(map[string]any); ok && m["id"] == safeID {
				return nil // already exists
			}
		}
		folders = append(folders, payload)
		cfg["folders"] = folders
		newCfg, _ := json.Marshal(cfg)
		putReq, _ := http.NewRequest("PUT", "http://127.0.0.1:8384/rest/config", strings.NewReader(string(newCfg)))
		putReq.Header.Set("Content-Type", "application/json")
		if strings.TrimSpace(apiKey) != "" {
			putReq.Header.Set("X-API-Key", apiKey)
		}
		putResp, _ := client.Do(putReq)
		if putResp != nil {
			_ = putResp.Body.Close()
		}
		return nil
	}
	_ = body
	return nil
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

func (d *Daemon) eventLoop() {
	defer d.wg.Done()
	// Initial reconcile immediately.
	d.reconcileOnce()
	reconcileTicker := time.NewTicker(10 * time.Minute)
	defer reconcileTicker.Stop()

	backoff := time.Second
	const maxBackoff = 30 * time.Second
	var lastID domain.ID
	d.mu.RLock()
	if len(d.events) > 0 {
		lastID = d.events[len(d.events)-1].ID
	}
	d.mu.RUnlock()

	for {
		if d.ctx.Err() != nil {
			return
		}
		if d.remote == nil {
			select {
			case <-d.ctx.Done():
				return
			case <-reconcileTicker.C:
				d.reconcileOnce()
			case <-time.After(30 * time.Second):
			}
			continue
		}
		ctx, cancel := context.WithCancel(d.ctx)
		ch := make(chan domain.Event, 32)
		errCh := make(chan error, 1)
		go func() {
			errCh <- d.remote.WatchCompanionEvents(ctx, lastID, ch)
		}()
		innerDone := false
		for !innerDone {
			select {
			case <-d.ctx.Done():
				cancel()
				return
			case <-reconcileTicker.C:
				d.reconcileOnce()
				backoff = time.Second
			case ev, ok := <-ch:
				if !ok {
					innerDone = true
					break
				}
				if ev.ID != "" {
					lastID = ev.ID
				}
				d.handleCompanionEvent(ev)
				backoff = time.Second
			case err := <-errCh:
				innerDone = true
				if err != nil && !errors.Is(err, context.Canceled) && d.ctx.Err() == nil {
					d.log.Warn("companion stream disconnected", "err", err, "backoff", backoff)
				}
			}
		}
		cancel()
		select {
		case <-d.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (d *Daemon) handleCompanionEvent(ev domain.Event) {
	switch ev.Type {
	case "environment.changed":
		_ = d.envSyncOnce()
		d.syncOnce()
		d.syncWebApps()
		_ = d.DaemonNotifyEvent(ev)
		d.broadcastStatus()
	case "apps.changed", "application.created", "application.updated", "application.deleted", "exposure.changed":
		d.syncWebApps()
		d.syncOnce()
		_ = d.DaemonNotifyEvent(ev)
		d.broadcastStatus()
	case "companion.revoked":
		d.mu.Lock()
		d.lastErr = "device revoked"
		d.mu.Unlock()
		d.removeWebApps()
		_ = d.DaemonNotifyEvent(ev)
		d.broadcastStatus()
	default:
		// For workspace.*, agent.awaiting_approval, etc.
		if strings.HasPrefix(ev.Type, "application.") || strings.HasPrefix(ev.Type, "exposure.") || ev.Type == "apps.changed" {
			d.syncWebApps()
		}
		// C3: desktop notifications for the events that matter
		if ShouldNotifyType(ev.Type) {
			_ = d.DaemonNotifyEvent(ev)
		}
		d.syncOnce()
		d.broadcastStatus()
	}
}

func (d *Daemon) syncWebApps() {
	if d.cfg == nil || strings.TrimSpace(d.cfg.ServerURL) == "" {
		return
	}
	if d.remote == nil {
		return
	}
	if auth, _ := d.remote.deviceAuthHeader(); auth == "" {
		return
	}
	if err := SyncWebApps(d.cfg.ServerURL); err != nil {
		d.log.Warn("webapp sync failed", "err", err)
	}
}

func (d *Daemon) removeWebApps() {
	if err := RemoveWebApps(); err != nil {
		d.log.Warn("webapp remove failed", "err", err)
	}
}

func (d *Daemon) reconcileOnce() {
	d.syncOnce()
	_ = d.envSyncOnce()
	if d.remote != nil {
		d.projects.FetchAll(d.ctx)
		ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
		defer cancel()
		var ps []domain.Project
		var err error
		if auth, _ := d.remote.deviceAuthHeader(); auth != "" {
			if p, err2 := d.remote.GetCompanionProjects(ctx); err2 == nil {
				ps = p
				err = nil
			} else if p2, err3 := d.remote.GetProjects(ctx); err3 == nil {
				ps = p2
				err = nil
			} else {
				err = err3
			}
		} else {
			ps, err = d.remote.GetProjects(ctx)
		}
		if err == nil {
			d.projects.SyncFromRemote(ps)
		}
	}
	d.syncWebApps()
	d.broadcastStatus()
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
	// E1: ensure omp MCP client config and OMAHAB_MCP_URL export.
	if d.cfg != nil && d.remote != nil {
		if tok, _ := d.creds.Get(CredentialService, CredentialDeviceAccount); strings.TrimSpace(tok) != "" {
			_ = EnsureMCPConfig(strings.TrimSpace(d.cfg.ServerURL), strings.TrimSpace(tok))
		}
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
	d.broadcastStatus()
	ticker := time.NewTicker(d.backupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			_ = d.backupStatusOnce()
			d.broadcastStatus()
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

	var st *domain.Status
	var evs []domain.Event
	var err error

	// Prefer companion endpoint when device token is available.
	if auth, _ := d.remote.deviceAuthHeader(); auth != "" {
		st, err = d.remote.GetCompanionStatus(ctx)
		if err == nil {
			if compEvs, eerr := d.remote.GetCompanionEvents(ctx); eerr == nil {
				evs = compEvs
			} else {
				d.log.Warn("companion events sync failed", "err", eerr)
			}
		} else {
			d.log.Warn("companion status sync failed, falling back to admin", "err", err)
			st, err = d.remote.GetStatus(ctx)
			if err == nil {
				if adminEvs, eerr := d.remote.GetEvents(ctx); eerr == nil {
					evs = adminEvs
				} else {
					d.log.Warn("events sync failed", "err", eerr)
				}
			}
		}
	} else {
		st, err = d.remote.GetStatus(ctx)
		if err == nil {
			if adminEvs, eerr := d.remote.GetEvents(ctx); eerr == nil {
				evs = adminEvs
			} else {
				d.log.Warn("events sync failed", "err", eerr)
			}
		}
	}
	if err != nil {
		d.mu.Lock()
		d.lastErr = err.Error()
		d.mu.Unlock()
		d.log.Warn("status sync failed", "err", err)
		return
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

	d.syncWebApps()
	d.maybeSelfUpdate()

	// Reconcile projects in background (non-blocking for status)
	go func() {
		pctx, pcancel := context.WithTimeout(d.ctx, 10*time.Second)
		defer pcancel()
		var ps []domain.Project
		var perr error
		if auth, _ := d.remote.deviceAuthHeader(); auth != "" {
			if compPs, err2 := d.remote.GetCompanionProjects(pctx); err2 == nil {
				ps = compPs
			} else if p2, err3 := d.remote.GetProjects(pctx); err3 == nil {
				ps = p2
			} else {
				perr = err3
			}
		} else {
			ps, perr = d.remote.GetProjects(pctx)
		}
		if perr == nil {
			d.projects.SyncFromRemote(ps)
		}
	}()
}
