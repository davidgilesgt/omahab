package api

// Bootstrap handlers: unauthenticated, first-boot only route group
// (/api/bootstrap/*) served on the LAN listener (:8485). The listener and
// routes exist only while /var/lib/omahab/bootstrap-done is absent.
import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)


// BootstrapGate is the control-plane side of first-boot bootstrap.
type BootstrapGate interface {
	// Claim validates the one-time code; success consumes it.
	Claim(code, sourceIP string) error
	// SSHKeys installs keys for the admin user.
	SSHKeys(githubUser string, pastedKeys []string) (int, error)
	// TailscaleUp starts enrollment, returning the auth URL.
	TailscaleUp() (string, error)
	// TailscaleStatus polls enrollment state.
	TailscaleStatus() (running bool, ip string, state string, err error)
	// Complete writes bootstrap-done and closes the listener.
	Complete() error
	// Active reports whether bootstrap is still pending.
	Active() bool
	// RestoreConnect verifies Hetzner/generic repo + phrase, uploads SSH key,
	// and lists snapshots. Returns up to 10 latest.
	RestoreConnect(ctx context.Context, req BootstrapRestoreConnectRequest) ([]BootstrapRestoreSnapshot, error)
	// RestoreRun starts the restore of snapshotID in background.
	RestoreRun(ctx context.Context, snapshotID string) error
	// RestoreEvents streams progress events for the running restore.
	RestoreEvents(ctx context.Context) <-chan BootstrapRestoreEvent
}

// BootstrapRestoreConnectRequest is the body for POST /api/bootstrap/restore/connect.
type BootstrapRestoreConnectRequest struct {
	Kind               string `json:"kind,omitempty"`
	Username           string `json:"username,omitempty"`
	Host               string `json:"host,omitempty"`
	SubAccountPassword string `json:"sub_account_password,omitempty"`
	Location           string `json:"location,omitempty"`
	Phrase             string `json:"phrase,omitempty"`
	PhraseWords        []string `json:"phrase_words,omitempty"`
}

// BootstrapRestoreSnapshot is one snapshot returned by restore/connect.
type BootstrapRestoreSnapshot struct {
	ID       string `json:"id"`
	Time     string `json:"time"`
	Hostname string `json:"hostname"`
}

// BootstrapRestoreRunRequest is the body for POST /api/bootstrap/restore/run.
type BootstrapRestoreRunRequest struct {
	SnapshotID string `json:"snapshot_id"`
}

// BootstrapRestoreEvent is one SSE event during restore.
type BootstrapRestoreEvent struct {
	Stage   string `json:"stage"`
	Message string `json:"message"`
	Done    bool   `json:"done"`
	Error   string `json:"error,omitempty"`
}


// handleBootstrapClaim exchanges the one-time code for the admin token.
func (s *Server) handleBootstrapClaim(w http.ResponseWriter, r *http.Request) {
	if s.bootstrap == nil || !s.bootstrap.Active() {
		writeError(w, r, newAPIError(http.StatusNotFound, CodeNotFound, "bootstrap complete"))
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, r, errBadRequest("invalid JSON body"))
		return
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		writeError(w, r, errBadRequest("code is required"))
		return
	}
	if err := s.bootstrap.Claim(code, clientIP(r)); err != nil {
		// Rate-limit exhaustion rotates the code; both cases are 429.
		if strings.Contains(err.Error(), "too many attempts") {
			writeError(w, r, newAPIError(http.StatusTooManyRequests, CodeUnprocessable, err.Error()))
			return
		}
		writeError(w, r, newAPIError(http.StatusUnauthorized, CodeUnprocessable, err.Error()))
		return
	}
	token := s.adminToken
	if token == "" {
		writeError(w, r, newAPIError(http.StatusInternalServerError, CodeInternal, "admin token unavailable"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

// handleBootstrapSSHKeys installs SSH keys (GitHub import or pasted).
func (s *Server) handleBootstrapSSHKeys(w http.ResponseWriter, r *http.Request) {
	if s.bootstrap == nil || !s.bootstrap.Active() {
		writeError(w, r, newAPIError(http.StatusNotFound, CodeNotFound, "bootstrap complete"))
		return
	}
	if !s.requireBootstrapToken(w, r) {
		return
	}
	var req struct {
		GitHubUser string   `json:"github_user"`
		Keys       []string `json:"keys"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, r, errBadRequest("invalid JSON body"))
		return
	}
	if strings.TrimSpace(req.GitHubUser) == "" && len(req.Keys) == 0 {
		// Skip is allowed: console access remains the recovery path.
		writeJSON(w, http.StatusOK, map[string]int{"added": 0})
		return
	}
	added, err := s.bootstrap.SSHKeys(strings.TrimSpace(req.GitHubUser), req.Keys)
	if err != nil {
		writeError(w, r, newAPIError(http.StatusBadRequest, CodeUnprocessable, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"added": added})
}

// handleBootstrapTailscaleUp starts Tailscale enrollment.
func (s *Server) handleBootstrapTailscaleUp(w http.ResponseWriter, r *http.Request) {
	if s.bootstrap == nil || !s.bootstrap.Active() {
		writeError(w, r, newAPIError(http.StatusNotFound, CodeNotFound, "bootstrap complete"))
		return
	}
	if !s.requireBootstrapToken(w, r) {
		return
	}
	url, err := s.bootstrap.TailscaleUp()
	if err != nil {
		// Not-yet-enrolled states return the URL with a 200; hard errors 502.
		writeError(w, r, newAPIError(http.StatusBadGateway, CodeInternal, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"auth_url": url})
}

// handleBootstrapTailscaleStatus polls Tailscale state.
func (s *Server) handleBootstrapTailscaleStatus(w http.ResponseWriter, r *http.Request) {
	if s.bootstrap == nil || !s.bootstrap.Active() {
		writeError(w, r, newAPIError(http.StatusNotFound, CodeNotFound, "bootstrap complete"))
		return
	}
	if !s.requireBootstrapToken(w, r) {
		return
	}
	running, ip, state, err := s.bootstrap.TailscaleStatus()
	if err != nil {
		writeError(w, r, newAPIError(http.StatusBadGateway, CodeInternal, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"running": running,
		"ip":      ip,
		"state":   state,
	})
}

// handleBootstrapComplete finishes bootstrap.
func (s *Server) handleBootstrapComplete(w http.ResponseWriter, r *http.Request) {
	if s.bootstrap == nil || !s.bootstrap.Active() {
		writeError(w, r, newAPIError(http.StatusNotFound, CodeNotFound, "bootstrap complete"))
		return
	}
	if !s.requireBootstrapToken(w, r) {
		return
	}
	if err := s.bootstrap.Complete(); err != nil {
		writeError(w, r, newAPIError(http.StatusInternalServerError, CodeInternal, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"complete": true})
}

// handleBootstrapRestoreConnect lists snapshots for restore.
func (s *Server) handleBootstrapRestoreConnect(w http.ResponseWriter, r *http.Request) {
	if s.bootstrap == nil || !s.bootstrap.Active() {
		writeError(w, r, newAPIError(http.StatusNotFound, CodeNotFound, "bootstrap complete"))
		return
	}
	if !s.requireBootstrapToken(w, r) {
		return
	}
	var req BootstrapRestoreConnectRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	snapshots, err := s.bootstrap.RestoreConnect(r.Context(), req)
	if err != nil {
		writeError(w, r, newAPIError(http.StatusBadGateway, CodeInternal, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": snapshots})
}

// handleBootstrapRestoreRun starts the restore.
func (s *Server) handleBootstrapRestoreRun(w http.ResponseWriter, r *http.Request) {
	if s.bootstrap == nil || !s.bootstrap.Active() {
		writeError(w, r, newAPIError(http.StatusNotFound, CodeNotFound, "bootstrap complete"))
		return
	}
	if !s.requireBootstrapToken(w, r) {
		return
	}
	var req BootstrapRestoreRunRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.SnapshotID) == "" {
		writeError(w, r, newAPIError(http.StatusBadRequest, CodeBadRequest, "snapshot_id is required"))
		return
	}
	if err := s.bootstrap.RestoreRun(r.Context(), req.SnapshotID); err != nil {
		writeError(w, r, newAPIError(http.StatusBadGateway, CodeInternal, err.Error()))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

// handleBootstrapRestoreEvents streams SSE progress.
func (s *Server) handleBootstrapRestoreEvents(w http.ResponseWriter, r *http.Request) {
	if s.bootstrap == nil || !s.bootstrap.Active() {
		writeError(w, r, newAPIError(http.StatusNotFound, CodeNotFound, "bootstrap complete"))
		return
	}
	if !s.requireBootstrapToken(w, r) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, newAPIError(http.StatusInternalServerError, CodeInternal, "sse not supported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ch := s.bootstrap.RestoreEvents(r.Context())
	enc := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			_, _ = w.Write([]byte("data: "))
			_ = enc.Encode(ev)
			_, _ = w.Write([]byte("\n"))
			flusher.Flush()
			if ev.Done {
				return
			}
		}
	}
}

// requireBootstrapToken validates the admin bearer obtained from claim.
func (s *Server) requireBootstrapToken(w http.ResponseWriter, r *http.Request) bool {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(auth, "Bearer ") {
		writeError(w, r, newAPIError(http.StatusUnauthorized, CodeUnauthorized, "missing bearer token"))
		return false
	}
	token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if s.adminToken == "" || token != s.adminToken {
		writeError(w, r, newAPIError(http.StatusUnauthorized, CodeUnauthorized, "invalid token"))
		return false
	}
	return true
}

func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}

// bootstrapGateActive middleware: 404 when bootstrap is not pending.
func (s *Server) bootstrapGateActive(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.bootstrap == nil || !s.bootstrap.Active() {
			writeError(w, r, newAPIError(http.StatusNotFound, CodeNotFound, "bootstrap complete"))
			return
		}
		next.ServeHTTP(w, r)
	})
}
