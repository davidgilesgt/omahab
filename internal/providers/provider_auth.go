package providers

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)
const (
	OAuthStatusConnecting       = "connecting"
	OAuthStatusNotAuthenticated = "not_authenticated"
	OAuthStatusNotEntitled      = "not_entitled"
	OAuthStatusQuotaExceeded    = "quota_exceeded"
)

// ProviderAuthAdapter owns one helper subprocess per provider. The helper
// (workers/provider-auth, same pinned LiteLLM environment as the gateway)
// imports the pinned authenticators and emits the NDJSON challenge protocol;
// the adapter owns cancellation/deadlines, safe metadata and opaque session
// IDs. UI never sees access/refresh tokens.
type ProviderAuthAdapter struct {
	helperPath string
	// dirs maps provider -> token directory (0700, fixed service user).
	dirs map[string]string
	// uid/gid run the helper as the fixed service user; negative skips setuid.
	uid int
	gid int

	mu       sync.Mutex
	procs    map[string]*authProc
	sessions map[string]*authSession
	closed   bool
}

type authProc struct {
	provider string
	cmd      *exec.Cmd
	cancel   context.CancelFunc
	done     chan struct{}
}

type authSession struct {
	id              string
	provider        string
	flow            string
	verificationURL string
	userCode        *string
	callbackPort    *int
	expiresAt       time.Time
	status          string
	errCode         string
	errMessage      string
	updatedAt       time.Time
}

// ProviderAuthOptions configures the adapter.
type ProviderAuthOptions struct {
	// HelperPath is the packaged omahab-provider-auth binary.
	HelperPath string
	// AuthDirs maps provider -> token directory.
	AuthDirs map[string]string
	// ServiceUser runs the helper as this fixed user (e.g. litellm).
	// Empty skips credential switching (dev/test).
	ServiceUser string
}

// NewProviderAuthAdapter builds the adapter. A missing helper binary fails
// Start calls, never construction, so status paths stay explanatory.
func NewProviderAuthAdapter(opts ProviderAuthOptions) *ProviderAuthAdapter {
	uid, gid := -1, -1
	if strings.TrimSpace(opts.ServiceUser) != "" {
		if u, err := user.Lookup(strings.TrimSpace(opts.ServiceUser)); err == nil {
			var parsedUID, parsedGID int
			if _, err := fmt.Sscanf(u.Uid, "%d", &parsedUID); err == nil {
				uid = parsedUID
			}
			if _, err := fmt.Sscanf(u.Gid, "%d", &parsedGID); err == nil {
				gid = parsedGID
			}
		}
	}
	dirs := map[string]string{}
	for k, v := range opts.AuthDirs {
		if strings.TrimSpace(v) != "" {
			dirs[strings.ToLower(k)] = v
		}
	}
	return &ProviderAuthAdapter{
		helperPath: strings.TrimSpace(opts.HelperPath),
		dirs:       dirs,
		uid:        uid,
		gid:        gid,
		procs:      map[string]*authProc{},
		sessions:   map[string]*authSession{},
	}
}

func helperSubcommand(provider string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case ProviderChatGPT:
		return "chatgpt-login", nil
	case ProviderXAI:
		return "xai-login", nil
	default:
		return "", fmt.Errorf("%w: unsupported provider %q", ErrValidation, provider)
	}
}

func providerFlow(provider string) string {
	if strings.ToLower(strings.TrimSpace(provider)) == ProviderChatGPT {
		return FlowDeviceCode
	}
	return FlowLoopback
}

func providerExpiry(provider string, now time.Time) time.Time {
	if strings.ToLower(strings.TrimSpace(provider)) == ProviderChatGPT {
		return now.Add(15 * time.Minute)
	}
	return now.Add(5 * time.Minute)
}

// Start launches one helper subprocess for the provider and blocks until the
// real device challenge arrives or the helper fails. There is no fake
// challenge: a missing binary reports not-configured.
func (a *ProviderAuthAdapter) Start(ctx context.Context, provider string) (OAuthSession, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	sub, err := helperSubcommand(provider)
	if err != nil {
		return OAuthSession{}, err
	}
	if strings.TrimSpace(a.helperPath) == "" {
		return OAuthSession{}, fmt.Errorf("%w: provider auth helper not configured", ErrValidation)
	}
	if _, err := os.Stat(a.helperPath); err != nil {
		return OAuthSession{}, fmt.Errorf("%w: provider auth helper unavailable: %v", ErrValidation, err)
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return OAuthSession{}, fmt.Errorf("%w: provider auth closed", ErrValidation)
	}
	if _, busy := a.procs[provider]; busy {
		a.mu.Unlock()
		return OAuthSession{}, fmt.Errorf("%w: %s login already in progress", ErrConflict, provider)
	}
	now := time.Now().UTC()
	sess := &authSession{
		id:        newID(),
		provider:  provider,
		flow:      providerFlow(provider),
		expiresAt: providerExpiry(provider, now),
		status:    OAuthStatusConnecting,
		updatedAt: now,
	}
	procCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, a.helperPath, sub)
	cmd.Dir = "/tmp"
	cmd.Env = a.helperEnv(provider)
	if a.uid >= 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(a.uid), Gid: uint32(a.gid)}, Setpgid: true}
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		a.mu.Unlock()
		return OAuthSession{}, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		cancel()
		a.mu.Unlock()
		return OAuthSession{}, err
	}
	proc := &authProc{provider: provider, cmd: cmd, cancel: cancel, done: make(chan struct{})}
	a.procs[provider] = proc
	a.sessions[sess.id] = sess
	id := sess.id
	a.mu.Unlock()

	go a.readLoop(proc, sess, stdout)

	// Block until the real challenge arrives, the helper fails, or the
	// caller gives up (cancellation reaps the child; saved credentials on
	// disk remain).
	waitCtx, waitCancel := context.WithTimeout(ctx, 90*time.Second)
	defer waitCancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		cur, alive, err := a.peek(id)
		if err != nil {
			return OAuthSession{}, err
		}
		if cur.verificationURL != "" || cur.status != OAuthStatusConnecting || !alive {
			return a.snapshot(id)
		}
		select {
		case <-waitCtx.Done():
			a.cancelSession(id)
			return OAuthSession{}, fmt.Errorf("%w: timed out waiting for provider challenge", ErrValidation)
		case <-ticker.C:
		}
	}
}

func (a *ProviderAuthAdapter) helperEnv(provider string) []string {
	env := []string{"PATH=/usr/bin:/bin", "HOME=/tmp", "LANG=C.UTF-8", "PYTHONUNBUFFERED=1"}
	if dir, ok := a.dirs[provider]; ok && strings.TrimSpace(dir) != "" {
		switch provider {
		case ProviderChatGPT:
			env = append(env, "CHATGPT_TOKEN_DIR="+dir)
		case ProviderXAI:
			env = append(env, "XAI_OAUTH_TOKEN_DIR="+dir)
		}
	}
	return env
}

func (a *ProviderAuthAdapter) peek(id string) (authSession, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	sess, ok := a.sessions[strings.TrimSpace(id)]
	if !ok {
		return authSession{}, false, ErrNotFound
	}
	_, alive := a.procs[sess.provider]
	return *sess, alive, nil
}

type helperEvent struct {
	Event           string  `json:"event"`
	Provider        string  `json:"provider"`
	VerificationURL string  `json:"verification_url"`
	UserCode        *string `json:"user_code"`
	ExpiresAt       float64 `json:"expires_at"`
	CallbackPort    *int    `json:"callback_port"`
	Code            string  `json:"code"`
	Message         string  `json:"message"`
	Authenticated   *bool   `json:"authenticated"`
}

func (a *ProviderAuthAdapter) readLoop(proc *authProc, sess *authSession, stdout io.Reader) {
	defer close(proc.done)
	defer func() {
		_ = proc.cmd.Wait()
		a.mu.Lock()
		if cur, ok := a.procs[proc.provider]; ok && cur == proc {
			delete(a.procs, proc.provider)
		}
		// A dead child with no terminal event expires the session; saved
		// credentials on disk remain for status/disconnect.
		if s, ok := a.sessions[sess.id]; ok && (s.status == OAuthStatusConnecting || s.status == OAuthStatusPending) {
			s.status = OAuthStatusExpired
			s.errCode = "expired"
			s.updatedAt = time.Now().UTC()
		}
		a.mu.Unlock()
	}()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev helperEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		a.mu.Lock()
		s, ok := a.sessions[sess.id]
		if !ok {
			a.mu.Unlock()
			return
		}
		switch ev.Event {
		case "challenge":
			if strings.TrimSpace(ev.VerificationURL) != "" {
				s.verificationURL = ev.VerificationURL
			}
			s.userCode = ev.UserCode
			s.callbackPort = ev.CallbackPort
			if ev.ExpiresAt > 0 {
				s.expiresAt = time.Unix(int64(ev.ExpiresAt), 0).UTC()
			}
			s.status = OAuthStatusConnecting
			s.updatedAt = time.Now().UTC()
		case "complete":
			s.status = OAuthStatusConnected
			s.errCode = ""
			s.errMessage = ""
			s.updatedAt = time.Now().UTC()
		case "error":
			s.status = mapHelperError(ev.Code)
			s.errCode = ev.Code
			s.errMessage = ev.Message
			s.updatedAt = time.Now().UTC()
		}
		a.mu.Unlock()
		if ev.Event == "complete" || ev.Event == "error" {
			return
		}
	}
}

func mapHelperError(code string) string {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "expired":
		return OAuthStatusExpired
	case "not_entitled":
		return OAuthStatusNotEntitled
	case "quota_exceeded":
		return OAuthStatusQuotaExceeded
	case "not_authenticated":
		return OAuthStatusNotAuthenticated
	default:
		return OAuthStatusError
	}
}

// snapshot returns the public session view. UI sees safe metadata only:
// verification URL, user code, expiry, status. Never tokens.
func (a *ProviderAuthAdapter) snapshot(id string) (OAuthSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	sess, ok := a.sessions[strings.TrimSpace(id)]
	if !ok {
		return OAuthSession{}, ErrNotFound
	}
	if time.Now().UTC().After(sess.expiresAt) && sess.status != OAuthStatusConnected {
		sess.status = OAuthStatusExpired
	}
	return OAuthSession{
		ID:              sess.id,
		Provider:        sess.provider,
		Flow:            sess.flow,
		VerificationURL: sess.verificationURL,
		UserCode:        sess.userCode,
		CallbackPort:    sess.callbackPort,
		ExpiresAt:       sess.expiresAt,
		Status:          sess.status,
	}, nil
}

// Poll returns the current session view.
func (a *ProviderAuthAdapter) Poll(_ context.Context, sessionID string) (OAuthSession, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return OAuthSession{}, fmt.Errorf("%w: session_id is required", ErrValidation)
	}
	return a.snapshot(sessionID)
}

func (a *ProviderAuthAdapter) cancelSession(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	sess, ok := a.sessions[strings.TrimSpace(id)]
	if !ok {
		return
	}
	sess.status = OAuthStatusExpired
	sess.errCode = "cancelled"
	sess.updatedAt = time.Now().UTC()
	if proc, ok := a.procs[sess.provider]; ok {
		proc.cancel()
	}
}

// Cancel reaps one session's child. Saved credentials on disk remain.
func (a *ProviderAuthAdapter) Cancel(_ context.Context, sessionID string) error {
	a.cancelSession(sessionID)
	return nil
}

// Close reaps every child: cancellation, timeout and daemon restart all
// converge here. Saved credentials remain.
func (a *ProviderAuthAdapter) Close() {
	a.mu.Lock()
	a.closed = true
	procs := make([]*authProc, 0, len(a.procs))
	for _, p := range a.procs {
		procs = append(procs, p)
	}
	a.mu.Unlock()
	for _, p := range procs {
		p.cancel()
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			if p.cmd.Process != nil {
				_ = p.cmd.Process.Kill()
			}
		}
	}
}

// runHelper runs a short-lived helper subcommand and parses NDJSON events.
func (a *ProviderAuthAdapter) runHelper(ctx context.Context, provider, sub string, timeout time.Duration) ([]helperEvent, error) {
	if strings.TrimSpace(a.helperPath) == "" {
		return nil, fmt.Errorf("%w: provider auth helper not configured", ErrValidation)
	}
	ctx2, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx2, a.helperPath, sub)
	cmd.Dir = "/tmp"
	cmd.Env = a.helperEnv(provider)
	if a.uid >= 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(a.uid), Gid: uint32(a.gid)}, Setpgid: true}
	}
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var events []helperEvent
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev helperEvent
		if err := json.Unmarshal([]byte(line), &ev); err == nil {
			events = append(events, ev)
		}
	}
	_ = cmd.Wait()
	return events, nil
}

// Capabilities runs helper import checks against the packaged environment.
func (a *ProviderAuthAdapter) Capabilities(ctx context.Context) (map[string]any, error) {
	events, err := a.runHelper(ctx, "", "capabilities", 60*time.Second)
	if err != nil {
		return nil, err
	}
	for _, ev := range events {
		if ev.Event == "capabilities" {
			out := map[string]any{}
			raw, _ := json.Marshal(ev)
			_ = json.Unmarshal(raw, &out)
			return out, nil
		}
		if ev.Event == "error" {
			return nil, fmt.Errorf("provider auth capabilities: %s: %s", ev.Code, ev.Message)
		}
	}
	return nil, fmt.Errorf("provider auth capabilities: no response")
}

// ConnectionStatus reports one provider's connection state without starting
// a login: token file + refresh proof, never a login loop.
func (a *ProviderAuthAdapter) ConnectionStatus(ctx context.Context, provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	var sub string
	switch provider {
	case ProviderChatGPT:
		sub = "chatgpt-status"
	case ProviderXAI:
		sub = "xai-status"
	default:
		return OAuthStatusError
	}
	if dir, ok := a.dirs[provider]; ok && strings.TrimSpace(dir) != "" {
		if _, err := os.Stat(filepath.Join(dir, "auth.json")); err != nil {
			return OAuthStatusNotAuthenticated
		}
	}
	events, err := a.runHelper(ctx, provider, sub, 60*time.Second)
	if err != nil {
		return OAuthStatusError
	}
	for _, ev := range events {
		if ev.Event == "status" && ev.Authenticated != nil {
			if *ev.Authenticated {
				return OAuthStatusConnected
			}
			return OAuthStatusExpired
		}
		if ev.Event == "error" {
			return mapHelperError(ev.Code)
		}
	}
	return OAuthStatusNotAuthenticated
}

// Disconnect revokes/deletes local saved auth via this provider-owned
// operation. It removes only the provider's own auth file; unrelated
// gateway credentials and models are never touched.
func (a *ProviderAuthAdapter) Disconnect(_ context.Context, provider string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	a.mu.Lock()
	for _, proc := range a.procs {
		if proc.provider == provider {
			proc.cancel()
		}
	}
	a.mu.Unlock()
	dir, ok := a.dirs[provider]
	if !ok || strings.TrimSpace(dir) == "" {
		return fmt.Errorf("%w: unknown provider %q", ErrValidation, provider)
	}
	target := filepath.Join(dir, "auth.json")
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
