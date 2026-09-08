package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/sshkeys"
)

// isGitHubUpstreamError classifies GitHub fetch failures as 502.
func isGitHubUpstreamError(err error) bool {
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "invalid github username") {
		return false
	}
	if strings.Contains(msg, "github") {
		// Empty, malformed, HTTP status, fetch, oversized, etc. are upstream.
		return true
	}
	if strings.Contains(msg, "fetch github") || strings.Contains(msg, "http") && strings.Contains(msg, "github") {
		return true
	}
	return false
}

func (s *Server) handleListSystemSSHKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.backend.ListSSHKeys(r.Context())
	if err != nil {
		writeError(w, r, newAPIError(http.StatusInternalServerError, CodeInternal, "failed to read SSH keys"))
		return
	}
	if keys == nil {
		keys = []sshkeys.SSHKey{}
	}
	username := s.backend.AdminUsername()
	writeJSON(w, http.StatusOK, map[string]any{
		"username": username,
		"items":    keys,
	})
}

func (s *Server) handleAddSystemSSHKeys(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GitHubUser string   `json:"github_user"`
		Keys       []string `json:"keys"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	added, err := s.backend.AddSSHKeys(r.Context(), strings.TrimSpace(req.GitHubUser), req.Keys)
	if err != nil {
		// Sanitize private key material from errors.
		msg := err.Error()
		if strings.Contains(strings.ToLower(msg), "private key") {
			msg = "private key not allowed"
		}
		if errors.Is(err, apitypes.ErrValidation) {
			writeError(w, r, newAPIError(http.StatusBadRequest, CodeBadRequest, msg))
			return
		}
		lower := strings.ToLower(msg)
		if strings.Contains(lower, "at least one") ||
			strings.Contains(lower, "no valid keys") ||
			strings.Contains(lower, "invalid pasted") ||
			strings.Contains(lower, "private key") ||
			strings.Contains(lower, "invalid github username") ||
			strings.Contains(lower, "username required") ||
			strings.Contains(lower, "no usable keys") {
			if strings.Contains(lower, "github") && strings.Contains(lower, "no usable keys") {
				writeError(w, r, newAPIError(http.StatusBadGateway, CodeInternal, msg))
				return
			}
			writeError(w, r, newAPIError(http.StatusBadRequest, CodeBadRequest, msg))
			return
		}
		if isGitHubUpstreamError(err) {
			writeError(w, r, newAPIError(http.StatusBadGateway, CodeInternal, msg))
			return
		}
		if strings.Contains(lower, "github") {
			writeError(w, r, newAPIError(http.StatusBadGateway, CodeInternal, msg))
			return
		}
		writeError(w, r, newAPIError(http.StatusInternalServerError, CodeInternal, "failed to add SSH keys"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"added": added})
}

func (s *Server) handleDeleteSystemSSHKeys(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Fingerprint string `json:"fingerprint"`
		ConfirmLast *bool  `json:"confirm_last"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	fp := strings.TrimSpace(req.Fingerprint)
	if fp == "" {
		writeError(w, r, newAPIError(http.StatusBadRequest, CodeBadRequest, "fingerprint is required"))
		return
	}
	confirm := false
	if req.ConfirmLast != nil {
		confirm = *req.ConfirmLast
	}
	err := s.backend.DeleteSSHKey(r.Context(), fp, confirm)
	if err != nil {
		if errors.Is(err, sshkeys.ErrLastKeyConfirmation) {
			writeError(w, r, newAPIError(http.StatusConflict, CodeConflict, err.Error()))
			return
		}
		if errors.Is(err, apitypes.ErrNotFound) {
			writeError(w, r, newAPIError(http.StatusNotFound, CodeNotFound, "ssh key not found"))
			return
		}
		if errors.Is(err, apitypes.ErrValidation) {
			writeError(w, r, newAPIError(http.StatusBadRequest, CodeBadRequest, err.Error()))
			return
		}
		// Without leaking fs details.
		writeError(w, r, newAPIError(http.StatusInternalServerError, CodeInternal, "failed to delete SSH key"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleBootstrapListSSHKeys(w http.ResponseWriter, r *http.Request) {
	if s.bootstrap == nil || !s.bootstrap.Active() {
		writeError(w, r, newAPIError(http.StatusNotFound, CodeNotFound, "bootstrap complete"))
		return
	}
	if !s.requireBootstrapToken(w, r) {
		return
	}
	keys, err := s.bootstrap.ListSSHKeys(r.Context())
	if err != nil {
		writeError(w, r, newAPIError(http.StatusInternalServerError, CodeInternal, "failed to read SSH keys"))
		return
	}
	if keys == nil {
		keys = []sshkeys.SSHKey{}
	}
	username := s.bootstrap.AdminUsername()
	writeJSON(w, http.StatusOK, map[string]any{
		"username": username,
		"items":    keys,
	})
}
