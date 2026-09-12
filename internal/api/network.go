package api

import (
	"net/http"
)

// Network handlers: home-LAN placement and explicit close-LAN state.
// All three routes live in the bearer-authenticated group.

// handleNetworkStatus reports placement, close-LAN state, and whether
// Tailscale is running. Tailscale errors read as down so status never
// fails when the binary is absent.
func (s *Server) handleNetworkStatus(w http.ResponseWriter, r *http.Request) {
	placement, closed, running := s.backend.NetworkStatus()
	writeJSON(w, http.StatusOK, map[string]any{
		"placement":         placement,
		"lan_closed":        closed,
		"tailscale_running": running,
	})
}

// handleCloseLAN deletes the three LAN :8484 nft rules and writes the
// close-LAN sentinel (sticky across reboots until reopened).
func (s *Server) handleCloseLAN(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.CloseLAN(); err != nil {
		writeError(w, r, newAPIError(http.StatusBadGateway, CodeInternal, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"closed": true})
}

// handleOpenLAN clears the sentinel and restarts nftables.service to
// restore the module table (including the LAN rules).
func (s *Server) handleOpenLAN(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.OpenLAN(); err != nil {
		writeError(w, r, newAPIError(http.StatusBadGateway, CodeInternal, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"closed": false})
}
