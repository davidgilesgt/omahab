package api

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/omahab/omahab/internal/domain"
)

// --- companion environment grants (admin, explicit owner consent) ---
//
// A granted enrolled device receives current tool-environment values on its
// next GET /api/v1/companion/environment fetch (rotations included, no
// re-grant needed); an ungranted or revoked device receives an empty bundle.
// Enrollment never grants. Responses carry consent state and revision only —
// never secret values.

// handleGrantDeviceEnvironment handles POST /api/v1/companion/devices/{id}/environment-grant (admin).
// The request body is ignored; the POST itself is the explicit consent record.
func (s *Server) handleGrantDeviceEnvironment(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	if strings.TrimSpace(string(id)) == "" {
		writeError(w, r, errBadRequest("id is required"))
		return
	}
	res, err := s.backend.GrantDeviceEnvironment(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleRevokeDeviceEnvironment handles DELETE /api/v1/companion/devices/{id}/environment-grant (admin).
// After revocation the device's next environment fetch returns empty.
func (s *Server) handleRevokeDeviceEnvironment(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	if strings.TrimSpace(string(id)) == "" {
		writeError(w, r, errBadRequest("id is required"))
		return
	}
	res, err := s.backend.RevokeDeviceEnvironment(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
