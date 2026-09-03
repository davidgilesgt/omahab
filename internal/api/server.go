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

	"github.com/omahab/omahab/internal/companion"
)

// Server is the Chi HTTP server for omahabd.
type Server struct {
	backend      Backend
	environments *companion.Service
	tokenHash    []byte // SHA256 of bearer token, nil means auth disabled (tests only)
	mcpTokenHash []byte // SHA256 of hermes_mcp_token, nil means MCP auth disabled
	emailHMACKey []byte
	scmWebhookSecret []byte
	version      string
	startedAt    time.Time
	router       chi.Router
	httpServer   *http.Server

	// bootstrap is the first-boot gate (LAN listener, /api/bootstrap/*).
	// nil disables the bootstrap routes.
	bootstrap BootstrapGate
	// adminToken is the raw admin API token returned by the bootstrap
	// claim (kept in memory only; never logged).
	adminToken string
	// mcpHandler is the streamable HTTP MCP handler for /mcp.
	mcpHandler http.Handler
}
type Config struct {
	Backend      Backend
	Environments *companion.Service
	Version      string
	BearerToken  string // raw token; hashed with SHA256 and compared constant-time
	MCPToken     string // raw hermes_mcp_token; hashed and checked by mcpAuth
	MCPHandler   http.Handler
	EmailHMACKey string // raw HMAC key for email webhook; empty disables HMAC check (tests)
	SCMWebhookSecret string // raw HMAC key for Forgejo webhook (platform-app/forgejo_webhook_secret)
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration
	BodyLimit    int64 // max JSON body bytes (default 1MiB)

	// Bootstrap enables the first-boot route group. Nil disables it.
	Bootstrap BootstrapGate
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
		backend:          cfg.Backend,
		environments:     cfg.Environments,
		version:          cfg.Version,
		startedAt:        time.Now().UTC(),
		emailHMACKey:     []byte(cfg.EmailHMACKey),
		scmWebhookSecret: []byte(cfg.SCMWebhookSecret),
		bootstrap:        cfg.Bootstrap,
		adminToken:       cfg.BearerToken,
		mcpHandler:       cfg.MCPHandler,
	}
	if cfg.BearerToken != "" {
		h := sha256.Sum256([]byte(cfg.BearerToken))
		s.tokenHash = h[:]
	}
	if cfg.MCPToken != "" {
		h := sha256.Sum256([]byte(cfg.MCPToken))
		s.mcpTokenHash = h[:]
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

	// SCM webhook from Forgejo (pull_request, push) — HMAC-verified, no bearer.
	r.Post("/api/v1/scm/webhook", s.withBodyLimit(defaultBodyLimit, s.handleSCMWebhook))

	// Companion enrollment: device claim with single-use code, no bearer (open). Code in JSON body {code}.
	r.Post("/api/v1/companion/enroll", s.withBodyLimit(defaultBodyLimit, s.handleEnrollCompanion))

	// MCP server for Hermes (outside bearerAuth, behind dedicated mcpAuth).
	// Supports POST/GET /mcp with Bearer OMAHAB_MCP_TOKEN.
	if s.mcpHandler != nil {
		r.Group(func(r chi.Router) {
			r.Use(s.mcpAuth)
			r.Handle("/mcp", s.mcpHandler)
			r.Handle("/mcp/", s.mcpHandler)
		})
	}

	// First-boot bootstrap (LAN listener only; group is inert when the
	// gate is nil or bootstrap already completed).
	r.Group(func(r chi.Router) {
		r.Post("/api/bootstrap/claim", s.withBodyLimit(defaultBodyLimit, s.handleBootstrapClaim))
		r.Group(func(r chi.Router) {
			r.Use(s.bootstrapGateActive)
			r.Post("/api/bootstrap/ssh-keys", s.withBodyLimit(64<<10, s.handleBootstrapSSHKeys))
			r.Post("/api/bootstrap/tailscale/up", s.handleBootstrapTailscaleUp)
			r.Get("/api/bootstrap/tailscale/status", s.handleBootstrapTailscaleStatus)
			r.Post("/api/bootstrap/complete", s.handleBootstrapComplete)
		})
	})

	// Authenticated API group.
	r.Group(func(r chi.Router) {
		r.Use(s.bearerAuth)

		// Enforce Content-Type application/json for mutating methods.
		r.Use(requireJSONContentType)

		// Status / instance / doctor
		r.Get("/api/v1/status", s.handleGetStatus)
		r.Get("/api/v1/instance", s.handleGetInstance)
		r.Put("/api/v1/instance", s.withBodyLimit(defaultBodyLimit, s.handleUpdateInstance))
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
		r.Post("/api/v1/workspaces/{id}/send", s.withBodyLimit(defaultBodyLimit, s.handleSendWorkspace))
		r.Post("/api/v1/workspaces/{id}/attach", s.handleAttachWorkspace)
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

		// Setup (first-run provisioning)
		r.Get("/api/v1/setup", s.handleGetSetup)
		r.Post("/api/v1/setup/verify-cloudflare", s.withBodyLimit(defaultBodyLimit, s.handleVerifyCloudflareToken))
		r.Post("/api/v1/recovery/generate", s.handleGenerateRecoveryKey)
		r.Post("/api/v1/recovery/confirm", s.withBodyLimit(defaultBodyLimit, s.handleConfirmRecoveryKey))
		r.Get("/api/v1/system/disks", s.handleListDisks)
		r.Put("/api/v1/system/storage", s.withBodyLimit(defaultBodyLimit, s.handleConfigureStorage))
		r.Get("/api/v1/backup-repositories", s.handleListBackupRepositories)
		r.Post("/api/v1/backup-repositories", s.withBodyLimit(defaultBodyLimit, s.handleCreateBackupRepository))
		r.Delete("/api/v1/backup-repositories/{id}", s.handleDeleteBackupRepository)
		r.Post("/api/v1/setup/reconcile", s.withBodyLimit(defaultBodyLimit, s.handleTriggerSetupReconcile))
		r.Put("/api/v1/setup/woodpecker", s.withBodyLimit(defaultBodyLimit, s.handleSetupWoodpecker))

		// Users / identity recovery
		r.Get("/api/v1/users", s.handleListUsers)
		r.Post("/api/v1/users", s.withBodyLimit(defaultBodyLimit, s.handleCreateUser))
		r.Get("/api/v1/users/{id}", s.handleGetUser)
		r.Patch("/api/v1/users/{id}", s.withBodyLimit(defaultBodyLimit, s.handleUpdateUser))
		r.Post("/api/v1/users/{id}/recovery", s.handleCreateUserRecovery)
		r.Post("/api/v1/users/{id}/enrollment", s.withBodyLimit(defaultBodyLimit, s.handleIssueUserEnrollment))
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

		// Model gateway (LiteLLM) — admin
		r.Get("/api/v1/model-aliases", s.handleListModelAliases)
		r.Put("/api/v1/model-aliases/{name}", s.withBodyLimit(defaultBodyLimit, s.handleSetModelAlias))
		r.Get("/api/v1/model-keys", s.handleListModelKeys)
		r.Post("/api/v1/model-keys", s.withBodyLimit(defaultBodyLimit, s.handleCreateModelKey))
		r.Delete("/api/v1/model-keys/{id}", s.handleDeleteModelKey)
		r.Post("/api/v1/provider-oauth/{provider}/start", s.withBodyLimit(defaultBodyLimit, s.handleStartProviderOAuth))
		r.Get("/api/v1/provider-oauth/{provider}/poll/{session_id}", s.handlePollProviderOAuth)

		// Companion — admin (enrollment codes, device lifecycle)
		r.Post("/api/v1/companion-enrollments", s.handleCreateCompanionEnrollment)
		r.Get("/api/v1/companion/devices", s.handleListCompanionDevices)
		r.Delete("/api/v1/companion/devices/{id}", s.handleRevokeCompanionDevice)
		r.Put("/api/v1/companion/devices/{id}/allow-oauth", s.withBodyLimit(defaultBodyLimit, s.handleSetDeviceAllowOAuth))

		// Tool environment (server authoritative singleton agent-tools) — admin
		r.Get("/api/v1/tool-environment", s.handleListToolEnv)
		r.Put("/api/v1/tool-environment/{NAME}", s.withBodyLimit(defaultBodyLimit, s.handlePutToolEnv))
		r.Delete("/api/v1/tool-environment/{NAME}", s.handleDeleteToolEnv)

		// Email (authenticated read + route activation gated on verification)
		r.Get("/api/v1/email/messages", s.handleListEmailMessages)
		r.Get("/api/v1/email/messages/{id}", s.handleGetEmailMessage)
		r.Post("/api/v1/email/routes", s.withBodyLimit(defaultBodyLimit, s.handleEnsureEmailRoute))
	})

	// Device-authenticated group — companion + callback relay.
	// deviceAuth validates Bearer oma_dev_... via environments.Service, sets device context,
	// rejects admin bearer with 403, and rejects missing/invalid device token with 401.
	r.Group(func(r chi.Router) {
		r.Use(s.deviceAuth)
		r.Get("/api/v1/companion/status", s.handleCompanionStatus)
		r.Get("/api/v1/companion/events", s.handleCompanionEvents)
		r.Get("/api/v1/companion/projects", s.handleCompanionProjects)
		r.Get("/api/v1/companion/workspaces", s.handleCompanionListWorkspaces)
		r.Post("/api/v1/companion/workspaces", s.withBodyLimit(defaultBodyLimit, s.handleCompanionCreateWorkspace))
		r.Get("/api/v1/companion/environment", s.handleGetCompanionEnvironment)
		r.Post("/api/v1/provider-oauth/{provider}/callback/{session_id}", s.withBodyLimit(defaultBodyLimit, s.handleForwardProviderOAuthCallback))
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
// which are not inside the authenticated group. Device tokens (oma_dev_...) are rejected with 403
// on every admin route to enforce allowlist: companion devices may only call device-endpoints.
func (s *Server) bearerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.tokenHash == nil {
			// No token configured; allow (used in tests). But still reject device tokens if they appear,
			// to preserve 403 semantics even when auth disabled? In tests tokenHash=nil means auth disabled,
			// so we allow all. However deviceAuth tests use nil tokenHash; bearerAuth with nil should still allow admin routes.
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
		// Enforce allowlist: device tokens never allowed on admin routes. Return 403 per spec.
		if strings.HasPrefix(token, "oma_dev_") {
			// If environments service is available, verify it is a valid device token before returning 403
			// to avoid leaking existence via timing, but always 403 for prefix match on admin.
			if s.environments != nil {
				if _, err := s.environments.ValidateDeviceToken(r.Context(), token); err == nil {
					writeError(w, r, errForbidden("device token not allowed on admin endpoint"))
					return
				}
			}
			writeError(w, r, errForbidden("device token not allowed on admin endpoint"))
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

// deviceAuth validates companion device tokens (oma_dev_...) via companion.Service.
// and on success sets device ID and device object in context.
func (s *Server) deviceAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Get("Authorization")
		if hdr == "" {
			writeError(w, r, errUnauthorized("missing device token"))
			return
		}
		parts := strings.SplitN(hdr, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			writeError(w, r, errUnauthorized("invalid authorization header"))
			return
		}
		token := strings.TrimSpace(parts[1])
		if token == "" {
			writeError(w, r, errUnauthorized("missing device token"))
			return
		}
		// Reject admin bearer on device endpoint with 403.
		if s.tokenHash != nil {
			got := sha256.Sum256([]byte(token))
			if subtle.ConstantTimeCompare(got[:], s.tokenHash) == 1 {
				writeError(w, r, errForbidden("admin bearer not allowed on device endpoint"))
				return
			}
		}
		if s.environments == nil {
			// No environments service (tests); allow any non-admin bearer through for now.
			next.ServeHTTP(w, r)
			return
		}
		dev, err := s.environments.ValidateDeviceToken(r.Context(), token)
		if err != nil {
		if errors.Is(err, companion.ErrRevoked) {
				writeError(w, r, errForbidden("device revoked"))
				return
			}
			writeError(w, r, errUnauthorized("invalid device token"))
			return
		}
		// Set device context for handlers.
		ctx := context.WithValue(r.Context(), companion.DeviceIDKey, dev.ID)
		ctx = context.WithValue(ctx, companion.DeviceKey, dev)
	})
}

// mcpAuth enforces the dedicated Hermes MCP token (platform-app/hermes_mcp_token).
// It is mounted at POST/GET /mcp outside bearerAuth. Missing token -> 401, wrong
// token -> 403, admin token and oma_dev_ tokens -> 403 with constant-time compare.
func (s *Server) mcpAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.mcpTokenHash == nil {
			// MCP auth disabled (no handler or no token) — in tests where mcpTokenHash is nil but handler exists, allow to surface 500? For hermetic tests, treat as unauthorized.
			// If no MCP token is configured, reject with 401 to avoid open endpoint in prod.
			writeError(w, r, errUnauthorized("mcp token not configured"))
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
		// Reject device tokens with 403
		if strings.HasPrefix(token, "oma_dev_") {
			writeError(w, r, errForbidden("device token not allowed on mcp endpoint"))
			return
		}
		// Reject admin bearer with 403 (constant-time)
		if s.tokenHash != nil {
			gotAdmin := sha256.Sum256([]byte(token))
			if subtle.ConstantTimeCompare(gotAdmin[:], s.tokenHash) == 1 {
				writeError(w, r, errForbidden("admin bearer not allowed on mcp endpoint"))
				return
			}
		}
		got := sha256.Sum256([]byte(token))
		if subtle.ConstantTimeCompare(got[:], s.mcpTokenHash) != 1 {
			writeError(w, r, errForbidden("invalid mcp token"))
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
