package api

// Bootstrap handlers: first-boot route group (/api/bootstrap/*).
// Token timing: token file provisioned only at handleBootstrapComplete
// (controlplane finalizeBootstrap); see controlplane/bootstrap_api.go
// decision: keep-at-Complete with explicit messaging at account-creation.
import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/controlplane"
	"github.com/omahab/omahab/internal/netenv"
)

type BootstrapGate = apitypes.BootstrapGate


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
	// No empty-code rejection here: the gate decides. An empty code from
	// a LAN source on lan placement claims successfully (first-claimer
	// wins); every other empty/wrong code fails with 401 plus rate-limit
	// accounting inside Claim.
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
		msg := err.Error()
		if strings.Contains(strings.ToLower(msg), "private key") {
			msg = "private key not allowed"
		}
		writeError(w, r, newAPIError(http.StatusBadRequest, CodeUnprocessable, msg))
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

// handleBootstrapStatus reports whether first-boot bootstrap is still active.
// Public (no auth), always available, with no-store. Also reports lan_claim:
// true when the caller may claim with an empty code (lan placement and a
// LAN source address) — the wizard uses it to offer a no-code claim button.
func (s *Server) handleBootstrapStatus(w http.ResponseWriter, r *http.Request) {
	active := s.bootstrap != nil && s.bootstrap.Active()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"active":    active,
		"lan_claim": active && controlplane.Placement() == "lan" && netenv.IsLANAddr(clientIP(r)),
	})
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
