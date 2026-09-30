package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/omahab/omahab/internal/apitypes"
)

// Custom bundle admin handlers (panel-token auth via the bearerAuth group;
// see server.go route registrations). All state changes go through the
// control-plane bundles service — the same internal/bundles.Service the
// `omahab bundle` CLI drives.

func (s *Server) handleListBundles(w http.ResponseWriter, r *http.Request) {
	items, err := s.backend.ListBundles(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleCreateBundle(w http.ResponseWriter, r *http.Request) {
	var req apitypes.CreateBundleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	item, err := s.backend.AddCustomBundle(r.Context(), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) handleDeleteBundle(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.backend.RemoveCustomBundle(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDisableBundle(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.SetCustomBundleDisabled(r.Context(), chi.URLParam(r, "id"), true); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleEnableBundle(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.SetCustomBundleDisabled(r.Context(), chi.URLParam(r, "id"), false); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleApplyBundles starts an apply (202 + operation) and returns the
// operation record immediately; poll GET /api/v1/operations/{id}.
func (s *Server) handleApplyBundles(w http.ResponseWriter, r *http.Request) {
	op, err := s.backend.TriggerBundleApply(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, op)
}

func (s *Server) handleGetBundleApply(w http.ResponseWriter, r *http.Request) {
	op, err := s.backend.GetBundleApplyStatus(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, op)
}
