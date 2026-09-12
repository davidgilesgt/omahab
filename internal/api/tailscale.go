package api

import (
	"net/http"
)

// handleTailscaleUp starts Tailscale enrollment on the authenticated API.
// Returns {"auth_url": url} with "" when already enrolled.
// Available any time (bearerAuth group; lan placement: LAN + tailnet sources bypass the token).
func (s *Server) handleTailscaleUp(w http.ResponseWriter, r *http.Request) {
	url, err := s.backend.TailscaleUp()
	if err != nil {
		writeError(w, r, newAPIError(http.StatusBadGateway, CodeInternal, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"auth_url": url})
}

// handleTailscaleStatus polls Tailscale state on the authenticated API.
// Shape: {running, ip, state}.
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
