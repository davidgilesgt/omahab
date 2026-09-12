package api

import (
	"net/http"
)

// handleTailscaleUp starts Tailscale enrollment on the authenticated API.
// Same behavior as POST /api/bootstrap/tailscale/up: returns {"auth_url": url}
// with "" when already enrolled. Unlike the bootstrap version it is available
// any time (bearerAuth group) and is not gated on first-boot.
func (s *Server) handleTailscaleUp(w http.ResponseWriter, r *http.Request) {
	url, err := s.backend.TailscaleUp()
	if err != nil {
		writeError(w, r, newAPIError(http.StatusBadGateway, CodeInternal, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"auth_url": url})
}

// handleTailscaleStatus polls Tailscale state on the authenticated API.
// Same shape as GET /api/bootstrap/tailscale/status: {running, ip, state}.
func (s *Server) handleTailscaleStatus(w http.ResponseWriter, r *http.Request) {
	running, ip, state, err := s.backend.TailscaleStatus()
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
