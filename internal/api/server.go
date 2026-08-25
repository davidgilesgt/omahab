package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Server is the Chi HTTP server for omahabd.
type Server struct {
	backend      Backend
	tokenHash    []byte // SHA256 of bearer token, nil means auth disabled (tests only)
	emailHMACKey []byte
	version      string
	startedAt    time.Time
	router       chi.Router
	httpServer   *http.Server
}

// Config configures the API server.
type Config struct {
	Backend      Backend
	Version      string
	BearerToken  string // raw token; hashed with SHA256 and compared constant-time
	EmailHMACKey string // raw HMAC key for email webhook; empty disables HMAC check (tests)
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration
	BodyLimit    int64 // max JSON body bytes (default 1MiB)
}

const (
	defaultBodyLimit    int64 = 1 << 20 // 1 MiB
	defaultReadTimeout        = 10 * time.Second
	defaultWriteTimeout       = 60 * time.Second
	defaultIdleTimeout        = 120 * time.Second
	maxBodyLimit        int64 = 36 << 20 // 25 MiB raw email plus base64/JSON overhead
)

// New creates a Server and its router. It does not start listening.
func New(cfg Config) (*Server, error) {
	if cfg.Backend == nil {
		return nil, errors.New("api: backend is required")
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = defaultReadTimeout
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = defaultWriteTimeout
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = defaultIdleTimeout
	}
	if cfg.BodyLimit == 0 {
		cfg.BodyLimit = defaultBodyLimit
	}

	s := &Server{
		backend:      cfg.Backend,
		version:      cfg.Version,
		startedAt:    time.Now().UTC(),
		emailHMACKey: []byte(cfg.EmailHMACKey),
	}
	if cfg.BearerToken != "" {
		h := sha256.Sum256([]byte(cfg.BearerToken))
		s.tokenHash = h[:]
	}
	s.router = s.buildRouter()
	s.httpServer = &http.Server{
		Handler:      s.router,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
	}
	return s, nil
}

// Router returns the chi Router.
func (s *Server) Router() chi.Router { return s.router }

// Handler returns the http.Handler (router with middleware).
func (s *Server) Handler() http.Handler { return s.router }

// HTTPServer returns the underlying http.Server for graceful shutdown.
func (s *Server) HTTPServer() *http.Server { return s.httpServer }

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

// buildRouter constructs all routes and middleware.
func (s *Server) buildRouter() chi.Router {
	r := chi.NewRouter()

	// Global middleware: request ID, recovery, real IP.
	r.Use(requestIDMiddleware)
	r.Use(safeRecovery)
	r.Use(middleware.RealIP)
	// Per-request timeout - skip SSE stream which is long-lived.
	r.Use(s.timeoutMiddleware(30 * time.Second))

	// Stable JSON error for unknown routes / methods.
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		writeError(w, req, errNotFound("not found"))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		writeError(w, req, newAPIError(http.StatusMethodNotAllowed, CodeNotFound, "method not allowed"))
	})

	// Public routes (no bearer auth).
	r.Get("/up", s.handleUp)

	// Release callback via per-project token (Woodpecker) — no admin bearer, verifies release token.
	r.Post("/api/v1/projects/{id}/releases/with-token", s.withBodyLimit(defaultBodyLimit, s.handleReleaseWithToken))

	// Email webhook with separate HMAC auth (not bearer).
	r.Post("/api/v1/email/ingest", s.withBodyLimit(maxBodyLimit, s.handleEmailIngestHMAC))
	// Authenticated API group.
	r.Group(func(r chi.Router) {
		r.Use(s.bearerAuth)

		// Enforce Content-Type application/json for mutating methods.
		r.Use(requireJSONContentType)

		// Status / instance / doctor
		r.Get("/api/v1/status", s.handleGetStatus)
		r.Get("/api/v1/instance", s.handleGetInstance)
		r.Get("/api/v1/doctor", s.handleDoctor)

		// Applications
		r.Get("/api/v1/catalog", s.handleListCatalog)
		r.Get("/api/v1/applications", s.handleListApplications)
		r.Post("/api/v1/applications", s.withBodyLimit(defaultBodyLimit, s.handleInstallApplication))
		r.Get("/api/v1/applications/{id}", s.handleGetApplication)
		r.Patch("/api/v1/applications/{id}", s.withBodyLimit(defaultBodyLimit, s.handleUpdateApplication))
		r.Post("/api/v1/applications/{id}/actions", s.withBodyLimit(defaultBodyLimit, s.handleApplicationAction))
		// Exposure
		r.Get("/api/v1/exposure", s.handleListExposure)
		r.Get("/api/v1/exposure/{kind}/{id}", s.handleGetExposure)
		r.Put("/api/v1/exposure/{kind}/{id}", s.withBodyLimit(defaultBodyLimit, s.handleUpdateExposure))
		// Convenience sub-resources
		r.Get("/api/v1/applications/{id}/exposure", s.handleGetApplicationExposure)
		r.Put("/api/v1/applications/{id}/exposure", s.withBodyLimit(defaultBodyLimit, s.handleUpdateApplicationExposure))
		r.Get("/api/v1/projects/{id}/exposure", s.handleGetProjectExposure)
		r.Put("/api/v1/projects/{id}/exposure", s.withBodyLimit(defaultBodyLimit, s.handleUpdateProjectExposure))

		// Projects & releases
		r.Get("/api/v1/projects", s.handleListProjects)
		r.Post("/api/v1/projects", s.withBodyLimit(defaultBodyLimit, s.handleCreateProject))
		r.Get("/api/v1/projects/{id}", s.handleGetProject)
		r.Patch("/api/v1/projects/{id}", s.withBodyLimit(defaultBodyLimit, s.handleUpdateProject))
		r.Delete("/api/v1/projects/{id}", s.handleDeleteProject)

		r.Get("/api/v1/projects/{id}/releases", s.handleListReleases)
		r.Post("/api/v1/projects/{id}/releases", s.withBodyLimit(defaultBodyLimit, s.handleCreateRelease))
		r.Get("/api/v1/projects/{id}/releases/{releaseID}", s.handleGetRelease)
		r.Post("/api/v1/projects/{id}/releases/{releaseID}/rollback", s.handleRollbackRelease)

		// Release tokens (admin only)
		r.Post("/api/v1/projects/{id}/release-token", s.handleIssueReleaseToken)
		r.Post("/api/v1/projects/{id}/release-token/rotate", s.handleRotateReleaseToken)

		// Push mirror config (repo-scoped credential, force-push warning)
		r.Get("/api/v1/projects/{id}/mirror", s.handleGetPushMirror)
		r.Put("/api/v1/projects/{id}/mirror", s.withBodyLimit(defaultBodyLimit, s.handleConfigurePushMirror))
		r.Delete("/api/v1/projects/{id}/mirror", s.handleRemovePushMirror)
		// Secrets (metadata only)
		r.Get("/api/v1/secrets", s.handleListSecrets)
		r.Post("/api/v1/secrets", s.withBodyLimit(defaultBodyLimit, s.handleCreateSecret))
		r.Get("/api/v1/secrets/{id}", s.handleGetSecret)
		r.Put("/api/v1/secrets/{id}", s.withBodyLimit(defaultBodyLimit, s.handleUpdateSecret))
		r.Delete("/api/v1/secrets/{id}", s.handleDeleteSecret)

		// Backups
		r.Get("/api/v1/backups", s.handleListBackups)
		r.Post("/api/v1/backups", s.withBodyLimit(defaultBodyLimit, s.handleCreateBackup))
		r.Get("/api/v1/backups/{id}", s.handleGetBackup)
		r.Post("/api/v1/backups/{id}/restore", s.handleRestoreBackup)
		r.Post("/api/v1/backups/{id}/verify", s.handleVerifyBackup)
		r.Post("/api/v1/backups/verify", s.handleVerifyLatestBackup)

		// Events + SSE
		r.Get("/api/v1/events", s.handleListEvents)
		r.Get("/api/v1/events/stream", s.handleStreamEvents)
		r.Get("/api/v1/events/{id}", s.handleGetEvent)
		r.Post("/api/v1/events/{id}/read", s.handleMarkEventRead)
		r.Post("/api/v1/events/read-all", s.handleMarkAllEventsRead)

		// Sync folders
		r.Get("/api/v1/sync/folders", s.handleListSyncFolders)
		r.Post("/api/v1/sync/folders", s.withBodyLimit(defaultBodyLimit, s.handleCreateSyncFolder))
		r.Get("/api/v1/sync/folders/{id}", s.handleGetSyncFolder)
		r.Patch("/api/v1/sync/folders/{id}", s.withBodyLimit(defaultBodyLimit, s.handleUpdateSyncFolder))
		r.Delete("/api/v1/sync/folders/{id}", s.handleDeleteSyncFolder)

		// Workspaces
		r.Get("/api/v1/workspaces", s.handleListWorkspaces)
		r.Post("/api/v1/workspaces", s.withBodyLimit(defaultBodyLimit, s.handleCreateWorkspace))
		r.Get("/api/v1/workspaces/{id}", s.handleGetWorkspace)
		r.Post("/api/v1/workspaces/{id}/stop", s.handleStopWorkspace)
		r.Delete("/api/v1/workspaces/{id}", s.handleDeleteWorkspace)
		r.Post("/api/v1/workspaces/{id}/capabilities", s.handleIssueWorkspaceCapability)
		r.Post("/api/v1/workspaces/{id}/capabilities/validate", s.withBodyLimit(defaultBodyLimit, s.handleValidateWorkspaceCapability))

		// Knowledge assistant tools (six + sources, index options, pinned models, consent)
		r.Post("/api/v1/knowledge/search", s.withBodyLimit(defaultBodyLimit, s.handleKnowledgeSearch))
		r.Get("/api/v1/knowledge/documents/{id}", s.handleKnowledgeGetDocument)
		r.Get("/api/v1/knowledge/documents/{id}/text", s.handleKnowledgeGetText)
		r.Get("/api/v1/knowledge/correspondents", s.handleKnowledgeListCorrespondents)
		r.Get("/api/v1/knowledge/document-types", s.handleKnowledgeListDocumentTypes)
		r.Get("/api/v1/knowledge/tags", s.handleKnowledgeListTags)
		r.Post("/api/v1/knowledge/upload", s.handleKnowledgeUpload)
		r.Post("/api/v1/knowledge/documents/{id}/tags", s.withBodyLimit(defaultBodyLimit, s.handleKnowledgeAddTag))
		r.Get("/api/v1/knowledge/sources", s.handleKnowledgeListSources)
		r.Get("/api/v1/knowledge/index-setup-options", s.handleKnowledgeIndexSetupOptions)
		r.Get("/api/v1/knowledge/pinned-models", s.handleKnowledgePinnedModels)
		r.Get("/api/v1/knowledge/consent", s.handleKnowledgeGetConsent)
		r.Put("/api/v1/knowledge/consent", s.withBodyLimit(defaultBodyLimit, s.handleKnowledgeSetConsent))

		// Users / identity recovery
		r.Get("/api/v1/users", s.handleListUsers)
		r.Post("/api/v1/users", s.withBodyLimit(defaultBodyLimit, s.handleCreateUser))
		r.Get("/api/v1/users/{id}", s.handleGetUser)
		r.Patch("/api/v1/users/{id}", s.withBodyLimit(defaultBodyLimit, s.handleUpdateUser))
		r.Delete("/api/v1/users/{id}", s.handleDeleteUser)
		r.Post("/api/v1/users/{id}/recovery", s.handleCreateUserRecovery)
		r.Post("/api/v1/identity/recover", s.withBodyLimit(defaultBodyLimit, s.handleIdentityRecover))
		r.Get("/api/v1/users/{id}/enrollment", s.handleGetEnrollmentState)
		r.Get("/api/v1/users/{id}/app-access", s.handleListApplicationAccess)
		r.Get("/api/v1/users/{id}/groups", s.handleGetUserGroups)
		r.Put("/api/v1/users/{id}/groups", s.withBodyLimit(defaultBodyLimit, s.handleSetUserGroups))
		// Provider credentials
		r.Get("/api/v1/provider-credentials", s.handleListProviderCredentials)
		r.Post("/api/v1/provider-credentials", s.withBodyLimit(defaultBodyLimit, s.handleCreateProviderCredential))
		r.Get("/api/v1/provider-credentials/{id}", s.handleGetProviderCredential)
		r.Delete("/api/v1/provider-credentials/{id}", s.handleDeleteProviderCredential)

		// Email (authenticated read + route activation gated on verification)
		r.Get("/api/v1/email/messages", s.handleListEmailMessages)
		r.Get("/api/v1/email/messages/{id}", s.handleGetEmailMessage)
		r.Post("/api/v1/email/routes", s.withBodyLimit(defaultBodyLimit, s.handleEnsureEmailRoute))
	})

	return r
}

// --- middleware ---

type ctxKey string

const requestIDKey ctxKey = "request_id"

// requestIDMiddleware ensures every request has an ID, echoed in response.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = newRequestID()
		}
		// Basic sanitization: strip newlines
		id = strings.ReplaceAll(id, "\n", "")
		id = strings.ReplaceAll(id, "\r", "")
		if len(id) > 128 {
			id = id[:128]
		}
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	// Format as hex 32 chars.
	return hex.EncodeToString(b[:])
}

// RequestIDFromContext returns the request ID stored in context.
func RequestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

// safeRecovery recovers from panics and returns 500 with stable envelope.
func safeRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				writeError(w, r, newAPIError(http.StatusInternalServerError, CodeInternal, "internal error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// timeoutMiddleware applies a per-request context timeout except for SSE.
func (s *Server) timeoutMiddleware(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/events/stream" {
				next.ServeHTTP(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// bearerAuth enforces constant-time bearer token comparison. Skipped only for /up and HMAC routes
// which are not inside the authenticated group.
func (s *Server) bearerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.tokenHash == nil {
			// No token configured; allow (used in tests).
			next.ServeHTTP(w, r)
			return
		}
		hdr := r.Header.Get("Authorization")
		if hdr == "" {
			writeError(w, r, errUnauthorized("missing bearer token"))
			return
		}
		parts := strings.SplitN(hdr, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			writeError(w, r, errUnauthorized("invalid authorization header"))
			return
		}
		token := strings.TrimSpace(parts[1])
		if token == "" {
			writeError(w, r, errUnauthorized("missing bearer token"))
			return
		}
		got := sha256.Sum256([]byte(token))
		if subtle.ConstantTimeCompare(got[:], s.tokenHash) != 1 {
			writeError(w, r, errUnauthorized("invalid bearer token"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireJSONContentType enforces Content-Type: application/json for mutating requests
// with a body. It runs inside the authenticated group; /up is outside.
func requireJSONContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
			if r.ContentLength != 0 {
				ct := r.Header.Get("Content-Type")
				if ct == "" {
					writeError(w, r, errUnsupportedMedia("Content-Type must be application/json"))
					return
				}
				// Allow application/json and application/json; charset=utf-8
				base := strings.Split(ct, ";")[0]
				base = strings.TrimSpace(strings.ToLower(base))
				if base != "application/json" {
					writeError(w, r, errUnsupportedMedia("Content-Type must be application/json"))
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// withBodyLimit wraps a handler with MaxBytesReader limiting.
func (s *Server) withBodyLimit(limit int64, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		h(w, r)
	}
}

// --- JSON helpers ---

// writeJSON writes a JSON response with status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	status := httpStatus(err)
	code := errorCode(err)
	msg := errorMessage(err)
	// Include request ID in log-safe way; never include secrets.
	_ = r
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": msg,
		},
	})
}

// decodeJSON decodes request JSON with unknown-field rejection and size already limited by MaxBytesReader.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		writeError(w, r, errBadRequest("missing request body"))
		return false
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, r, errPayloadTooLarge("request body too large"))
			return false
		}
		// Detect unknown field
		if strings.Contains(err.Error(), "unknown field") {
			writeError(w, r, errUnknownField(err.Error()))
			return false
		}
		if errors.Is(err, io.EOF) {
			writeError(w, r, errBadRequest("empty request body"))
			return false
		}
		writeError(w, r, errInvalidJSON(err.Error()))
		return false
	}
	// Ensure no trailing data
	if dec.More() {
		writeError(w, r, errInvalidJSON("trailing data after JSON object"))
		return false
	}
	return true
}

// parsePagination parses limit/offset or page/per_page. Defaults limit=50, max 100.
func parsePagination(r *http.Request) Pagination {
	q := r.URL.Query()
	limit := 50
	offset := 0

	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			limit = n
		}
	}
	if v := q.Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			limit = n
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	if v := q.Get("page"); v != "" {
		ppStr := q.Get("per_page")
		pp := limit
		if ppStr != "" {
			if n, err := strconv.Atoi(ppStr); err == nil && n > 0 {
				pp = n
			}
		}
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = (n - 1) * pp
			limit = pp
		}
	}
	if limit > 100 {
		limit = 100
	}
	if limit < 0 {
		limit = 0
	}
	if offset < 0 {
		offset = 0
	}
	return Pagination{Limit: limit, Offset: offset}
}

// parseID extracts domain.ID from URL param.
func parseID(param string) string { return param }
