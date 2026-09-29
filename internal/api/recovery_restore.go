package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/omahab/omahab/internal/apitypes"
)

// Offline disaster-recovery endpoints for the setup-UI "Restore from
// backup" flow (DESIGN §5.4, §24.6). All routes live in the panel-token
// authenticated group. The recovery phrase crosses the wire exactly once
// per call, is held in memory only, and is never logged or persisted.

// handleRecoveryListSnapshots lists candidate snapshots for a restore:
// POST /api/v1/recovery/restore/snapshots with phrase + repository.
func (s *Server) handleRecoveryListSnapshots(w http.ResponseWriter, r *http.Request) {
	var req apitypes.RecoveryListRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	out, err := s.backend.ListRecoverySnapshots(r.Context(), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRecoveryPrepareRestore stages and validates a server-config
// snapshot without touching live state: POST /api/v1/recovery/restore.
// The swap itself runs in the omahab-restore oneshot unit with omahabd
// stopped, triggered by the apply route.
func (s *Server) handleRecoveryPrepareRestore(w http.ResponseWriter, r *http.Request) {
	var req apitypes.RecoveryRestoreRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	op, err := s.backend.PrepareRecoveryRestore(r.Context(), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, op)
}

// handleRecoveryApplyRestore triggers the omahab-restore oneshot unit for
// a staged restore: POST /api/v1/recovery/restore/{id}/apply with the
// recovery phrase. The unit stops omahabd, swaps state atomically as
// root, and restarts omahabd — the API cannot run the swap itself while
// serving.
func (s *Server) handleRecoveryApplyRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phrase string `json:"phrase"`
	}
	if r.ContentLength != 0 {
		if !decodeJSON(w, r, &req) {
			return
		}
	}
	op, err := s.backend.TriggerRecoveryRestoreUnit(r.Context(), chi.URLParam(r, "id"), req.Phrase)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, op)
}

// handleGetRecoveryRestore polls one offline restore:
// GET /api/v1/recovery/restore/{id}.
func (s *Server) handleGetRecoveryRestore(w http.ResponseWriter, r *http.Request) {
	op, err := s.backend.GetRecoveryRestoreStatus(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, op)
}
