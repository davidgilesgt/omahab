package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/emailing"
)

// list envelope used by dashboard. Matches web/src/api/types.ts ListEnvelope<T> {items}
type listEnvelope[T any] struct {
	Items []T  `json:"items"`
	Total *int `json:"total,omitempty"`
}

func writeList[T any](w http.ResponseWriter, items []T) {
	if items == nil {
		items = []T{}
	}
	writeJSON(w, http.StatusOK, listEnvelope[T]{Items: items})
}

// --- /up and status ---

func (s *Server) handleUp(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "up",
		"version": s.version,
	})
}

func (s *Server) handleGetStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.backend.GetStatus(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleGetInstance(w http.ResponseWriter, r *http.Request) {
	inst, err := s.backend.GetInstance(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, inst)
}

func (s *Server) handleUpdateInstance(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Domain        string `json:"domain"`
		AssistantName string `json:"assistant_name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	inst, err := s.backend.UpdateInstance(r.Context(), req.Domain, req.AssistantName)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, inst)
}

func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	rep, err := s.backend.GetDoctor(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// --- applications ---

func (s *Server) handleListApplications(w http.ResponseWriter, r *http.Request) {
	p := parsePagination(r)
	items, err := s.backend.ListApplications(r.Context(), p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleGetApplication(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	app, err := s.backend.GetApplication(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, app)
}

func (s *Server) handleUpdateApplication(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	var req UpdateApplicationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Exposure != nil && !req.Exposure.Valid() {
		writeError(w, r, errBadRequest("invalid exposure"))
		return
	}
	app, err := s.backend.UpdateApplication(r.Context(), id, req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, app)
}

func (s *Server) handleApplicationAction(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	var req ApplicationActionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Action) == "" {
		writeError(w, r, errBadRequest("action is required"))
		return
	}
	app, err := s.backend.DoApplicationAction(r.Context(), id, req.Action)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, app)
}

func (s *Server) handleInstallApplication(w http.ResponseWriter, r *http.Request) {
	var req InstallApplicationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.BundleID) == "" {
		writeError(w, r, errBadRequest("bundle_id is required"))
		return
	}
	if req.Exposure != "" && !req.Exposure.Valid() {
		writeError(w, r, errBadRequest("invalid exposure"))
		return
	}
	app, err := s.backend.InstallApplication(r.Context(), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, app)
}

// --- catalog ---

func (s *Server) handleListCatalog(w http.ResponseWriter, r *http.Request) {
	items, err := s.backend.ListCatalog(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, items)
}

// --- exposure ---

func (s *Server) handleListExposure(w http.ResponseWriter, r *http.Request) {
	items, err := s.backend.ListExposure(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleGetExposure(w http.ResponseWriter, r *http.Request) {
	kind := chi.URLParam(r, "kind")
	id := domain.ID(chi.URLParam(r, "id"))
	st, err := s.backend.GetExposure(r.Context(), kind, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleUpdateExposure(w http.ResponseWriter, r *http.Request) {
	kind := chi.URLParam(r, "kind")
	id := domain.ID(chi.URLParam(r, "id"))
	var req UpdateExposureRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !req.Exposure.Valid() {
		writeError(w, r, errBadRequest("invalid exposure"))
		return
	}
	if err := s.confirmPublicExposure(r, kind, id, req); err != nil {
		writeError(w, r, err)
		return
	}
	st, err := s.backend.UpdateExposure(r.Context(), kind, id, req.Exposure)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// confirmPublicExposure enforces typed confirmation when making a resource
// public: the caller must repeat the exact hostname being exposed.
func (s *Server) confirmPublicExposure(r *http.Request, kind string, id domain.ID, req UpdateExposureRequest) error {
	if req.Exposure != domain.ExposurePublic {
		return nil
	}
	st, err := s.backend.GetExposure(r.Context(), kind, id)
	if err != nil {
		return err
	}
	if strings.EqualFold(strings.TrimSpace(req.Confirmation), strings.TrimSpace(st.Hostname)) && req.Confirmation != "" {
		return nil
	}
	return errConfirmation("public exposure requires confirmation matching the hostname: " + st.Hostname)
}

func (s *Server) handleGetApplicationExposure(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	st, err := s.backend.GetExposure(r.Context(), "application", id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleUpdateApplicationExposure(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	var req UpdateExposureRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !req.Exposure.Valid() {
		writeError(w, r, errBadRequest("invalid exposure"))
		return
	}
	if err := s.confirmPublicExposure(r, "application", id, req); err != nil {
		writeError(w, r, err)
		return
	}
	st, err := s.backend.UpdateExposure(r.Context(), "application", id, req.Exposure)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleGetProjectExposure(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	st, err := s.backend.GetExposure(r.Context(), "project", id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleUpdateProjectExposure(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	var req UpdateExposureRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !req.Exposure.Valid() {
		writeError(w, r, errBadRequest("invalid exposure"))
		return
	}
	if err := s.confirmPublicExposure(r, "project", id, req); err != nil {
		writeError(w, r, err)
		return
	}
	st, err := s.backend.UpdateExposure(r.Context(), "project", id, req.Exposure)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// --- projects ---

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	p := parsePagination(r)
	items, err := s.backend.ListProjects(r.Context(), p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	proj, err := s.backend.GetProject(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, proj)
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var req CreateProjectRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" && strings.TrimSpace(req.Slug) == "" {
		writeError(w, r, errBadRequest("slug or name is required"))
		return
	}
	if req.Exposure != "" && !req.Exposure.Valid() {
		writeError(w, r, errBadRequest("invalid exposure"))
		return
	}
	proj, err := s.backend.CreateProject(r.Context(), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, proj)
}

func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	var req UpdateProjectRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Exposure != nil && !req.Exposure.Valid() {
		writeError(w, r, errBadRequest("invalid exposure"))
		return
	}
	proj, err := s.backend.UpdateProject(r.Context(), id, req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, proj)
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	if err := s.backend.DeleteProject(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- releases ---

func (s *Server) handleListReleases(w http.ResponseWriter, r *http.Request) {
	pid := domain.ID(chi.URLParam(r, "id"))
	p := parsePagination(r)
	items, err := s.backend.ListReleases(r.Context(), pid, p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleGetRelease(w http.ResponseWriter, r *http.Request) {
	pid := domain.ID(chi.URLParam(r, "id"))
	rid := domain.ID(chi.URLParam(r, "releaseID"))
	rel, err := s.backend.GetRelease(r.Context(), pid, rid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rel)
}

func (s *Server) handleCreateRelease(w http.ResponseWriter, r *http.Request) {
	pid := domain.ID(chi.URLParam(r, "id"))
	var req CreateReleaseRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Commit) == "" || strings.TrimSpace(req.Digest) == "" {
		writeError(w, r, errBadRequest("commit and digest are required"))
		return
	}
	rel, err := s.backend.CreateRelease(r.Context(), pid, req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, rel)
}

func (s *Server) handleRollbackRelease(w http.ResponseWriter, r *http.Request) {
	pid := domain.ID(chi.URLParam(r, "id"))
	rid := domain.ID(chi.URLParam(r, "releaseID"))
	rel, err := s.backend.RollbackRelease(r.Context(), pid, rid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rel)
}

// --- secrets (metadata only) ---

func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	p := parsePagination(r)
	scope := r.URL.Query().Get("scope")
	items, err := s.backend.ListSecrets(r.Context(), scope, p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleCreateSecret(w http.ResponseWriter, r *http.Request) {
	var req CreateSecretRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Scope) == "" || strings.TrimSpace(req.Name) == "" || req.Value == "" {
		writeError(w, r, errBadRequest("scope, name and value are required"))
		return
	}
	sec, err := s.backend.CreateSecret(r.Context(), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, sec)
}

func (s *Server) handleGetSecret(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	sec, err := s.backend.GetSecret(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sec)
}

func (s *Server) handleUpdateSecret(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	var req UpdateSecretRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Value == "" {
		writeError(w, r, errBadRequest("value is required"))
		return
	}
	sec, err := s.backend.UpdateSecret(r.Context(), id, req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sec)
}

func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	if err := s.backend.DeleteSecret(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- backups ---

func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	p := parsePagination(r)
	items, err := s.backend.ListBackups(r.Context(), p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleGetBackup(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	bk, err := s.backend.GetBackup(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, bk)
}

func (s *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	var req CreateBackupRequest
	// Allow empty body for default repo; if body present decode.
	if r.ContentLength != 0 {
		if !decodeJSON(w, r, &req) {
			return
		}
	}
	bk, err := s.backend.CreateBackup(r.Context(), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, bk)
}

func (s *Server) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	bk, err := s.backend.RestoreBackup(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, bk)
}

func (s *Server) handleVerifyBackup(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	bk, err := s.backend.VerifyBackup(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, bk)
}

func (s *Server) handleVerifyLatestBackup(w http.ResponseWriter, r *http.Request) {
	bk, err := s.backend.VerifyBackup(r.Context(), "")
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, bk)
}

// --- events ---

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	p := parsePagination(r)
	q := r.URL.Query()
	filter := EventFilter{
		Type:     q.Get("type"),
		Severity: q.Get("severity"),
	}
	if v := q.Get("unread"); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			// unread=true means ReadAt is nil; backend interprets.
			tmp := b
			filter.Unread = &tmp
		}
	}
	items, err := s.backend.ListEvents(r.Context(), p, filter)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleGetEvent(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	ev, err := s.backend.GetEvent(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

func (s *Server) handleMarkEventRead(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	ev, err := s.backend.MarkEventRead(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

func (s *Server) handleMarkAllEventsRead(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.MarkAllEventsRead(r.Context()); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- sync folders ---

func (s *Server) handleListSyncFolders(w http.ResponseWriter, r *http.Request) {
	p := parsePagination(r)
	items, err := s.backend.ListSyncFolders(r.Context(), p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleGetSyncFolder(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	f, err := s.backend.GetSyncFolder(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) handleCreateSyncFolder(w http.ResponseWriter, r *http.Request) {
	var req CreateSyncFolderRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.ServerPath) == "" {
		writeError(w, r, errBadRequest("name and server_path are required"))
		return
	}
	f, err := s.backend.CreateSyncFolder(r.Context(), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

func (s *Server) handleUpdateSyncFolder(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	var req UpdateSyncFolderRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	f, err := s.backend.UpdateSyncFolder(r.Context(), id, req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) handleDeleteSyncFolder(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	if err := s.backend.DeleteSyncFolder(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- workspaces ---

func (s *Server) handleListWorkspaces(w http.ResponseWriter, r *http.Request) {
	p := parsePagination(r)
	items, err := s.backend.ListWorkspaces(r.Context(), p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleGetWorkspace(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	ws, err := s.backend.GetWorkspace(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	var req CreateWorkspaceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ProjectID == "" {
		writeError(w, r, errBadRequest("project_id is required"))
		return
	}
	ws, err := s.backend.CreateWorkspace(r.Context(), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, ws)
}

func (s *Server) handleStopWorkspace(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	ws, err := s.backend.StopWorkspace(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

func (s *Server) handleDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	if err := s.backend.DeleteWorkspace(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- users / identity ---

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	p := parsePagination(r)
	items, err := s.backend.ListUsers(r.Context(), p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	u, err := s.backend.GetUser(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req CreateUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Email) == "" {
		writeError(w, r, errBadRequest("email is required"))
		return
	}
	u, err := s.backend.CreateUser(r.Context(), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	var req UpdateUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	u, err := s.backend.UpdateUser(r.Context(), id, req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	if err := s.backend.DeleteUser(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCreateUserRecovery(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	rec, err := s.backend.CreateUserRecoverySession(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (s *Server) handleIssueUserEnrollment(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	u, err := s.backend.IssueUserEnrollment(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) handleIdentityRecover(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Email) == "" {
		writeError(w, r, errBadRequest("email is required"))
		return
	}
	rec, err := s.backend.CreateRecoverySession(r.Context(), req.Email)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

// --- provider credentials ---

func (s *Server) handleListProviderCredentials(w http.ResponseWriter, r *http.Request) {
	p := parsePagination(r)
	items, err := s.backend.ListProviderCredentials(r.Context(), p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleGetProviderCredential(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	cred, err := s.backend.GetProviderCredential(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cred)
}

func (s *Server) handleCreateProviderCredential(w http.ResponseWriter, r *http.Request) {
	var req CreateProviderCredentialRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Provider) == "" || strings.TrimSpace(req.Kind) == "" {
		writeError(w, r, errBadRequest("provider and kind are required"))
		return
	}
	if req.Value == "" {
		writeError(w, r, errBadRequest("value is required"))
		return
	}
	cred, err := s.backend.CreateProviderCredential(r.Context(), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// Never echo value.
	writeJSON(w, http.StatusCreated, cred)
}

func (s *Server) handleDeleteProviderCredential(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	if err := s.backend.DeleteProviderCredential(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- model gateway (LiteLLM) ---

func (s *Server) handleListModelAliases(w http.ResponseWriter, r *http.Request) {
	items, err := s.backend.ListModelAliases(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleSetModelAlias(w http.ResponseWriter, r *http.Request) {
	raw := chi.URLParam(r, "name")
	// chi keeps %2F encoded; decode to handle omahab/* aliases.
	name := raw
	if decoded, err := url.PathUnescape(raw); err == nil {
		name = decoded
	}
	if name != "omahab/fast" && name != "omahab/balanced" && name != "omahab/reasoning" && name != "omahab/embedding" {
		writeError(w, r, errBadRequest("invalid alias name"))
		return
	}
	var req SetModelAliasRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.CredentialID) == "" || strings.TrimSpace(req.Model) == "" {
		writeError(w, r, errBadRequest("credential_id and model are required"))
		return
	}
	alias, err := s.backend.SetModelAlias(r.Context(), name, req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, alias)
}

func (s *Server) handleListModelKeys(w http.ResponseWriter, r *http.Request) {
	p := parsePagination(r)
	items, err := s.backend.ListModelKeys(r.Context(), p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleCreateModelKey(w http.ResponseWriter, r *http.Request) {
	var req CreateModelKeyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.OwnerKind) == "" || strings.TrimSpace(req.OwnerID) == "" {
		writeError(w, r, errBadRequest("name, owner_kind and owner_id are required"))
		return
	}
	if req.OwnerKind != "hermes" && req.OwnerKind != "device" && req.OwnerKind != "harness" {
		writeError(w, r, errBadRequest("invalid owner_kind"))
		return
	}
	key, token, err := s.backend.CreateModelKey(r.Context(), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// Return metadata plus plaintext key once. Never persist plaintext.
	type createResp struct {
		ModelKey
		Key string `json:"key"`
	}
	writeJSON(w, http.StatusCreated, createResp{ModelKey: key, Key: token})
}

func (s *Server) handleDeleteModelKey(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	if err := s.backend.DeleteModelKey(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStartProviderOAuth(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	if provider != "chatgpt" && provider != "xai" {
		writeError(w, r, errBadRequest("unsupported provider"))
		return
	}
	var req StartProviderOAuthRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Flow != "device_code" && req.Flow != "loopback" {
		writeError(w, r, errBadRequest("invalid flow"))
		return
	}
	// Enforce sanctioned provider/flow pairs: chatgpt=device_code, xai=loopback
	if provider == "chatgpt" && req.Flow != "device_code" {
		writeError(w, r, errBadRequest("chatgpt requires device_code flow"))
		return
	}
	if provider == "xai" && req.Flow != "loopback" {
		writeError(w, r, errBadRequest("xai requires loopback flow"))
		return
	}
	sess, err := s.backend.StartProviderOAuth(r.Context(), provider, req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// Ensure no secret fields leak: OAuthSession only contains safe fields.
	writeJSON(w, http.StatusCreated, sess)
}

func (s *Server) handlePollProviderOAuth(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	sessionID := chi.URLParam(r, "session_id")
	if provider != "chatgpt" && provider != "xai" {
		writeError(w, r, errBadRequest("unsupported provider"))
		return
	}
	if strings.TrimSpace(sessionID) == "" {
		writeError(w, r, errBadRequest("session_id is required"))
		return
	}
	sess, err := s.backend.PollProviderOAuth(r.Context(), provider, sessionID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) handleForwardProviderOAuthCallback(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	sessionID := chi.URLParam(r, "session_id")
	if provider != "xai" {
		writeError(w, r, errBadRequest("callback only for xai loopback flow"))
		return
	}
	if strings.TrimSpace(sessionID) == "" {
		writeError(w, r, errBadRequest("session_id is required"))
		return
	}
	var req ForwardProviderOAuthCallbackRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// Validate callback_path: must be exactly /callback?<query> with no host/scheme/port and no shell metachars.
	cp := strings.TrimSpace(req.CallbackPath)
	if cp == "" || !strings.HasPrefix(cp, "/callback") {
		writeError(w, r, errBadRequest("callback_path must start with /callback"))
		return
	}
	// Reject absolute URLs, host injection, scheme, port
	if strings.Contains(cp, "://") || strings.Contains(cp, "\\") {
		writeError(w, r, errBadRequest("invalid callback_path"))
		return
	}
	// Reject shell metacharacters that could be used for command injection
	if strings.ContainsAny(cp, "|;&$`'\"*?~<>^()[]{}!\\") && !strings.Contains(cp, "?") {
		// Allow ? and = & % for query, but reject obvious shell fragments
		// More precise: reject ; | & $ ` ' " \ and similar outside query encoding
	}
	if strings.Contains(cp, ";") || strings.Contains(cp, "|") || strings.Contains(cp, "$") || strings.Contains(cp, "`") || strings.Contains(cp, "\"") || strings.Contains(cp, "'") || strings.Contains(cp, "\\") {
		writeError(w, r, errBadRequest("callback_path contains forbidden characters"))
		return
	}
	// Ensure it is a path, not a full URL: must start with / and not contain host
	if strings.HasPrefix(cp, "//") {
		writeError(w, r, errBadRequest("invalid callback_path"))
		return
	}
	sess, err := s.backend.ForwardProviderOAuthCallback(r.Context(), provider, sessionID, req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

// --- companion enrollment & devices ---

func (s *Server) handleCreateCompanionEnrollment(w http.ResponseWriter, r *http.Request) {
	enr, code, err := s.backend.CreateCompanionEnrollment(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	// Return id, code (once), expires_at, created_at. Code is returned once and never stored plaintext.
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         enr.ID,
		"code":       code,
		"expires_at": enr.ExpiresAt,
		"created_at": enr.CreatedAt,
	})
}

func (s *Server) handleEnrollCompanion(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		writeError(w, r, errBadRequest("code is required"))
		return
	}
	// Code may not be bearer; it's single-use enrollment code. Reject NUL/CR/LF already validated in service.
	token, err := s.backend.EnrollCompanion(r.Context(), code)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// Return token once: oma_dev_... . Stored only as hash server-side.
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":        token,
		"token_prefix": token[:16],
	})
}

func (s *Server) handleListCompanionDevices(w http.ResponseWriter, r *http.Request) {
	p := parsePagination(r)
	items, err := s.backend.ListCompanionDevices(r.Context(), p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleRevokeCompanionDevice(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	if strings.TrimSpace(string(id)) == "" {
		writeError(w, r, errBadRequest("id is required"))
		return
	}
	if err := s.backend.RevokeCompanionDevice(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSetDeviceAllowOAuth(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	if strings.TrimSpace(string(id)) == "" {
		writeError(w, r, errBadRequest("id is required"))
		return
	}
	var req struct {
		Allow bool `json:"allow"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	dev, err := s.backend.SetDeviceAllowOAuth(r.Context(), id, req.Allow)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, dev)
}

func (s *Server) handleCompanionStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.backend.GetStatus(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleCompanionEvents(w http.ResponseWriter, r *http.Request) {
	p := parsePagination(r)
	// Companion events: same as admin events but filtered; for now return all.
	items, err := s.backend.ListEvents(r.Context(), p, EventFilter{})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleCompanionProjects(w http.ResponseWriter, r *http.Request) {
	p := parsePagination(r)
	items, err := s.backend.ListProjects(r.Context(), p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, items)
}

// --- tool-environment (admin, server authoritative singleton agent-tools) ---

func (s *Server) handleListToolEnv(w http.ResponseWriter, r *http.Request) {
	items, err := s.backend.ListToolEnvironments(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handlePutToolEnv(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "NAME")
	if strings.TrimSpace(name) == "" {
		name = chi.URLParam(r, "name")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		writeError(w, r, errBadRequest("name is required"))
		return
	}
	var req PutToolEnvRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// Handler-layer NUL/CR/LF rejection so persistent one-line systemd assignments are unambiguous.
	if strings.Contains(req.Value, "\x00") || strings.Contains(req.Value, "\r") || strings.Contains(req.Value, "\n") {
		writeError(w, r, errBadRequest("value must not contain NUL, CR, or LF"))
		return
	}
	meta, err := s.backend.PutToolEnvironment(r.Context(), name, req.Value)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, meta)
}

func (s *Server) handleDeleteToolEnv(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "NAME")
	if strings.TrimSpace(name) == "" {
		name = chi.URLParam(r, "name")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		writeError(w, r, errBadRequest("name is required"))
		return
	}
	if err := s.backend.DeleteToolEnvironment(r.Context(), name); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetCompanionEnvironment(w http.ResponseWriter, r *http.Request) {
	// Device-authenticated only: admin bearer already rejected with 403 in deviceAuth middleware.
	// Extract Bearer token for backend validation and ETag derivation.
	hdr := r.Header.Get("Authorization")
	token := ""
	if parts := strings.SplitN(hdr, " ", 2); len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		token = strings.TrimSpace(parts[1])
	}
	if token == "" {
		writeError(w, r, errUnauthorized("missing device token"))
		return
	}
	bundle, etag, err := s.backend.GetCompanionEnvironment(r.Context(), token)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// ETag derived from revision/device key (e.g., W/"rev-<revision>-<deviceIDHash>")
	w.Header().Set("ETag", etag)
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		// Weak comparison: If-None-Match matches ETag or is *.
		if inm == etag || inm == "*" || strings.TrimSpace(inm) == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		// Also compare without weak prefix for client compatibility
		if strings.TrimPrefix(inm, "W/") == strings.TrimPrefix(etag, "W/") {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	// Return raw values only under device auth. Never log values.
	writeJSON(w, http.StatusOK, bundle)
}

// --- email ingestion ---

// handleEmailIngestHMAC authenticates the Cloudflare Email Worker's v1
// canonical envelope before handing the raw message to the control plane.
func (s *Server) handleEmailIngestHMAC(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, r, errPayloadTooLarge("request body too large"))
			return
		}
		writeError(w, r, errBadRequest(err.Error()))
		return
	}
	if len(bytes.TrimSpace(body)) == 0 {
		writeError(w, r, errBadRequest("empty request body"))
		return
	}

	var payload struct {
		From      string `json:"from"`
		To        string `json:"to"`
		Timestamp string `json:"timestamp"`
		Nonce     string `json:"nonce"`
		Raw       string `json:"raw"`
		RawSize   int    `json:"rawSize"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			writeError(w, r, errUnknownField(err.Error()))
			return
		}
		writeError(w, r, errInvalidJSON(err.Error()))
		return
	}
	if payload.From == "" || payload.To == "" || payload.Timestamp == "" || payload.Nonce == "" || payload.Raw == "" {
		writeError(w, r, errBadRequest("from, to, timestamp, nonce, and raw are required"))
		return
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(payload.Raw)
	if err != nil {
		writeError(w, r, errBadRequest("raw must be canonical base64"))
		return
	}
	if payload.RawSize != len(raw) {
		writeError(w, r, errBadRequest("rawSize does not match decoded raw length"))
		return
	}

	signature := r.Header.Get("X-Omahab-Signature")
	if r.Header.Get("X-Omahab-Timestamp") != payload.Timestamp ||
		r.Header.Get("X-Omahab-Nonce") != payload.Nonce ||
		r.Header.Get("X-Omahab-From") != payload.From ||
		r.Header.Get("X-Omahab-To") != payload.To {
		writeError(w, r, errUnauthorized("signed metadata headers do not match payload"))
		return
	}
	if len(s.emailHMACKey) != 0 && !emailing.VerifyHMACV1(
		s.emailHMACKey, payload.Timestamp, payload.Nonce, payload.From, payload.To, raw, signature,
	) {
		writeError(w, r, errUnauthorized("invalid HMAC signature"))
		return
	}

	msg, err := s.backend.IngestEmail(r.Context(), EmailIngestRequest{
		From: payload.From, To: payload.To, Timestamp: payload.Timestamp,
		Nonce: payload.Nonce, Raw: raw, RawSize: payload.RawSize, Signature: signature,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, msg)
}

func (s *Server) handleListEmailMessages(w http.ResponseWriter, r *http.Request) {
	p := parsePagination(r)
	items, err := s.backend.ListEmailMessages(r.Context(), p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleGetEmailMessage(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	msg, err := s.backend.GetEmailMessage(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

// --- release tokens (admin only) ---

func (s *Server) handleIssueReleaseToken(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	resp, err := s.backend.IssueReleaseToken(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleRotateReleaseToken(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	resp, err := s.backend.RotateReleaseToken(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleReleaseWithToken(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	// Extract Bearer token from Authorization header for per-project release token
	auth := r.Header.Get("Authorization")
	token := ""
	if strings.HasPrefix(auth, "Bearer ") {
		token = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	} else {
		token = r.Header.Get("X-Release-Token")
		if token == "" {
			token = r.URL.Query().Get("token")
		}
	}
	if token == "" {
		writeError(w, r, errUnauthorized("release token is required"))
		return
	}
	var req CreateReleaseRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	rel, err := s.backend.ReleaseWithToken(r.Context(), id, token, req.Commit, req.Digest)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, rel)
}

// --- push mirror ---

func (s *Server) handleGetPushMirror(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	m, err := s.backend.GetPushMirror(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleConfigurePushMirror(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	var req ConfigureMirrorRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	m, err := s.backend.ConfigurePushMirror(r.Context(), id, req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleRemovePushMirror(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(chi.URLParam(r, "id"))
	if err := s.backend.RemovePushMirror(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- workspace capabilities ---

func (s *Server) handleIssueWorkspaceCapability(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	resp, err := s.backend.IssueWorkspaceCapability(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleValidateWorkspaceCapability(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Token) == "" {
		writeError(w, r, errBadRequest("token is required"))
		return
	}
	if err := s.backend.ValidateWorkspaceCapability(r.Context(), id, req.Token); err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": true})
}

// --- knowledge assistant tools ---

func (s *Server) handleKnowledgeSearch(w http.ResponseWriter, r *http.Request) {
	var req KnowledgeSearchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	principal := req.Principal
	if principal == "" {
		principal = r.URL.Query().Get("principal")
	}
	if principal == "" {
		principal = r.Header.Get("X-Principal")
	}
	cits, err := s.backend.KnowledgeSearch(r.Context(), principal, req.Query, req.Limit)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, cits)
}

func (s *Server) handleKnowledgeGetDocument(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principal := r.URL.Query().Get("principal")
	if principal == "" {
		principal = r.Header.Get("X-Principal")
	}
	meta, err := s.backend.KnowledgeGetMetadata(r.Context(), principal, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// also include text if available
	txt, _ := s.backend.KnowledgeGetText(r.Context(), principal, id)
	writeJSON(w, http.StatusOK, map[string]any{"metadata": meta, "text": txt})
}

func (s *Server) handleKnowledgeGetText(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principal := r.URL.Query().Get("principal")
	if principal == "" {
		principal = r.Header.Get("X-Principal")
	}
	txt, err := s.backend.KnowledgeGetText(r.Context(), principal, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": txt})
}

func (s *Server) handleKnowledgeListCorrespondents(w http.ResponseWriter, r *http.Request) {
	principal := r.URL.Query().Get("principal")
	if principal == "" {
		principal = r.Header.Get("X-Principal")
	}
	list, err := s.backend.KnowledgeListCorrespondents(r.Context(), principal)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, list)
}

func (s *Server) handleKnowledgeListDocumentTypes(w http.ResponseWriter, r *http.Request) {
	principal := r.URL.Query().Get("principal")
	if principal == "" {
		principal = r.Header.Get("X-Principal")
	}
	list, err := s.backend.KnowledgeListDocumentTypes(r.Context(), principal)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, list)
}

func (s *Server) handleKnowledgeListTags(w http.ResponseWriter, r *http.Request) {
	principal := r.URL.Query().Get("principal")
	if principal == "" {
		principal = r.Header.Get("X-Principal")
	}
	list, err := s.backend.KnowledgeListTags(r.Context(), principal)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, list)
}

func (s *Server) handleKnowledgeUpload(w http.ResponseWriter, r *http.Request) {
	// content type may be multipart or json; for test we accept json with base64
	if strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			writeError(w, r, errBadRequest("invalid multipart"))
			return
		}
		filename := r.FormValue("filename")
		if filename == "" {
			filename = "upload.bin"
		}
		principal := r.FormValue("principal")
		if principal == "" {
			principal = r.URL.Query().Get("principal")
		}
		var content []byte
		if fh, _, err := r.FormFile("file"); err == nil {
			content, _ = io.ReadAll(fh)
			_ = fh.Close()
		}
		tags := r.Form["tags"]
		id, err := s.backend.KnowledgeUpload(r.Context(), principal, filename, content, tags)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id})
		return
	}
	var req KnowledgeUploadRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	id, err := s.backend.KnowledgeUpload(r.Context(), req.Principal, req.Filename, req.Content, req.Tags)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Server) handleKnowledgeAddTag(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principal := r.URL.Query().Get("principal")
	if principal == "" {
		principal = r.Header.Get("X-Principal")
	}
	var req struct {
		Tag       string `json:"tag"`
		Principal string `json:"principal"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Principal != "" {
		principal = req.Principal
	}
	if strings.TrimSpace(req.Tag) == "" {
		writeError(w, r, errBadRequest("tag is required"))
		return
	}
	if err := s.backend.KnowledgeAddTag(r.Context(), principal, id, req.Tag); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleKnowledgeListSources(w http.ResponseWriter, r *http.Request) {
	list, err := s.backend.KnowledgeListSources(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, list)
}

func (s *Server) handleKnowledgeIndexSetupOptions(w http.ResponseWriter, r *http.Request) {
	opts, err := s.backend.KnowledgeIndexSetupOptions(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, opts)
}

func (s *Server) handleKnowledgePinnedModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.backend.KnowledgePinnedModels(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, models)
}

func (s *Server) handleKnowledgeGetConsent(w http.ResponseWriter, r *http.Request) {
	principal := r.URL.Query().Get("principal")
	provider := r.URL.Query().Get("provider")
	if principal == "" || provider == "" {
		writeError(w, r, errBadRequest("principal and provider are required"))
		return
	}
	has, err := s.backend.KnowledgeGetSummarizationConsent(r.Context(), principal, provider)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"principal": principal, "provider": provider, "granted": has})
}

func (s *Server) handleKnowledgeSetConsent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Principal string `json:"principal"`
		Provider  string `json:"provider"`
		Granted   bool   `json:"granted"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Principal) == "" || strings.TrimSpace(req.Provider) == "" {
		writeError(w, r, errBadRequest("principal and provider are required"))
		return
	}
	if err := s.backend.KnowledgeSetSummarizationConsent(r.Context(), req.Principal, req.Provider, req.Granted); err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"principal": req.Principal, "provider": req.Provider, "granted": req.Granted})
}

// --- identity extended ---

func (s *Server) handleGetEnrollmentState(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	st, err := s.backend.GetEnrollmentState(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleListApplicationAccess(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	list, err := s.backend.ListApplicationAccess(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, list)
}

func (s *Server) handleGetUserGroups(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	groups, err := s.backend.GetUserGroups(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, groups)
}

func (s *Server) handleSetUserGroups(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		GroupIDs []string `json:"group_ids"`
		Groups   []string `json:"groups"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	gids := req.GroupIDs
	if len(gids) == 0 {
		gids = req.Groups
	}
	if err := s.backend.SetUserGroups(r.Context(), id, gids); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- setup ---

func (s *Server) handleGetSetup(w http.ResponseWriter, r *http.Request) {
	st, err := s.backend.GetSetupStatus(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleTriggerSetupReconcile(w http.ResponseWriter, r *http.Request) {
	already, err := s.backend.TriggerSetupReconcile(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if already {
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "reconciling", "already_running": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "reconciling", "already_running": false})
}

func (s *Server) handleSetupWoodpecker(w http.ResponseWriter, r *http.Request) {
	var req SetupWoodpeckerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	username := strings.TrimSpace(req.Username)
	token := strings.TrimSpace(req.Token)
	if username == "" || token == "" {
		writeError(w, r, errBadRequest("username and token are required"))
		return
	}
	if len(username) < 1 || len(username) > 255 {
		writeError(w, r, errBadRequest("username must be 1-255 characters"))
		return
	}
	if len(token) < 1 || len(token) > 4096 {
		writeError(w, r, errBadRequest("token must be 1-4096 characters"))
		return
	}
	// Strict bounded non-empty inputs already checked; delegate to backend for Woodpecker validation.
	// Never log token. Pass trimmed values.
	trimmed := SetupWoodpeckerRequest{Username: username, Token: token}
	if err := s.backend.SetupWoodpecker(r.Context(), trimmed); err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// --- email routes ---

func (s *Server) handleEnsureEmailRoute(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Recipient string `json:"recipient"`
	}
	if r.ContentLength > 0 {
		if !decodeJSON(w, r, &req) {
			return
		}
	}
	if req.Recipient == "" {
		req.Recipient = r.URL.Query().Get("recipient")
	}
	if err := s.backend.EnsureEmailRoute(r.Context(), req.Recipient); err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleVerifyCloudflareToken runs the live token check server-side.
func (s *Server) handleVerifyCloudflareToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	res, err := s.backend.VerifyCloudflareToken(r.Context(), req.Token)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleGenerateRecoveryKey creates a one-time age key pair + kit.
func (s *Server) handleGenerateRecoveryKey(w http.ResponseWriter, r *http.Request) {
	mat, err := s.backend.GenerateRecoveryKey(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, mat)
}

// handleConfirmRecoveryKey persists the confirmed kit.
func (s *Server) handleConfirmRecoveryKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PublicKey string `json:"public_key"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.backend.ConfirmRecoveryKey(r.Context(), req.PublicKey); err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"confirmed": true})
}

// handleListDisks lists candidate storage filesystems.
func (s *Server) handleListDisks(w http.ResponseWriter, r *http.Request) {
	disks, err := s.backend.ListDisks(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, disks)
}

// handleConfigureStorage assigns a filesystem to a volume role.
func (s *Server) handleConfigureStorage(w http.ResponseWriter, r *http.Request) {
	var req ConfigureStorageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.backend.ConfigureStorage(r.Context(), req); err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"configured": true})
}

// handleListBackupRepositories lists configured backup repositories.
func (s *Server) handleListBackupRepositories(w http.ResponseWriter, r *http.Request) {
	repos, err := s.backend.ListBackupRepositories(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeList(w, repos)
}

// handleCreateBackupRepository stores credentials + configures the repo.
func (s *Server) handleCreateBackupRepository(w http.ResponseWriter, r *http.Request) {
	var req CreateBackupRepositoryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	repo, err := s.backend.CreateBackupRepository(r.Context(), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, repo)
}

// handleDeleteBackupRepository removes a repository.
func (s *Server) handleDeleteBackupRepository(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.backend.DeleteBackupRepository(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
