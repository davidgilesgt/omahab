package domain

import "time"

// ID is an opaque, stable control-plane identifier.
type ID string

type Health string

const (
	HealthUnknown   Health = "unknown"
	HealthHealthy   Health = "healthy"
	HealthDegraded  Health = "degraded"
	HealthUnhealthy Health = "unhealthy"
)

type Exposure string

const (
	ExposurePrivate Exposure = "private"
	ExposureShared  Exposure = "shared"
	ExposurePublic  Exposure = "public"
)

func (e Exposure) Valid() bool {
	return e == ExposurePrivate || e == ExposureShared || e == ExposurePublic
}

type Instance struct {
	ID            ID        `json:"id"`
	Domain        string    `json:"domain"`
	Tailnet       string    `json:"tailnet"`
	TailscaleIP   string    `json:"tailscale_ip"`
	AssistantName string    `json:"assistant_name"`
	AssistantSlug string    `json:"assistant_slug"`
	CreatedAt     time.Time `json:"created_at"`
}

type Status struct {
	InstanceID ID        `json:"instance_id"`
	Version    string    `json:"version"`
	Health     Health    `json:"health"`
	StartedAt  time.Time `json:"started_at"`
	Now        time.Time `json:"now"`
}

type Application struct {
	ID            ID         `json:"id"`
	Name          string     `json:"name"`
	BundleID      string     `json:"bundle_id"`
	Image         string     `json:"image"`
	Digest        string     `json:"digest"`
	Hostname      string     `json:"hostname"`
	Exposure      Exposure   `json:"exposure"`
	Health        Health     `json:"health"`
	DesiredState  string     `json:"desired_state"`
	ObservedState string     `json:"observed_state"`
	InstalledAt   *time.Time `json:"installed_at,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type Project struct {
	ID            ID        `json:"id"`
	Slug          string    `json:"slug"`
	Name          string    `json:"name"`
	RepositoryURL string    `json:"repository_url"`
	Exposure      Exposure  `json:"exposure"`
	Hostname      string    `json:"hostname"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type Release struct {
	ID        ID        `json:"id"`
	ProjectID ID        `json:"project_id"`
	Commit    string    `json:"commit"`
	Digest    string    `json:"digest"`
	Status    string    `json:"status"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Secret struct {
	ID        ID        `json:"id"`
	Scope     string    `json:"scope"`
	Name      string    `json:"name"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Backup struct {
	ID         ID         `json:"id"`
	Repository string     `json:"repository"`
	SnapshotID string     `json:"snapshot_id,omitempty"`
	Status     string     `json:"status"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      string     `json:"error,omitempty"`
}

type Event struct {
	ID         ID             `json:"id"`
	Type       string         `json:"type"`
	Severity   string         `json:"severity"`
	ResourceID ID             `json:"resource_id,omitempty"`
	Message    string         `json:"message"`
	Data       map[string]any `json:"data,omitempty"`
	ReadAt     *time.Time     `json:"read_at,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
}

type SyncFolder struct {
	ID          ID        `json:"id"`
	Name        string    `json:"name"`
	ServerPath  string    `json:"server_path"`
	ShareWithAI bool      `json:"share_with_ai"`
	Health      Health    `json:"health"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Workspace struct {
	ID           ID         `json:"id"`
	ProjectID    ID         `json:"project_id"`
	Branch       string     `json:"branch"`
	Title        string     `json:"title,omitempty"`
	Instructions string     `json:"instructions,omitempty"`
	Agent        string     `json:"agent"`
	Status       string     `json:"status"`
	Capability   string     `json:"-"`
	LastActiveAt time.Time  `json:"last_active_at"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	GatewayKeyID *ID        `json:"gateway_key_id,omitempty"`
}

type EmailMessage struct {
	ID             ID        `json:"id"`
	EnvelopeFrom   string    `json:"envelope_from"`
	HeaderFrom     string    `json:"header_from"`
	Recipient      string    `json:"recipient"`
	Subject        string    `json:"subject"`
	Authentication string    `json:"authentication"`
	Status         string    `json:"status"`
	ReceivedAt     time.Time `json:"received_at"`
}

type User struct {
	ID                  ID         `json:"id"`
	Email               string     `json:"email"`
	Name                string     `json:"name"`
	Groups              []string   `json:"groups"`
	Disabled            bool       `json:"disabled"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	EnrollmentURL       *string    `json:"enrollment_url,omitempty"`
	EnrollmentExpiresAt *time.Time `json:"enrollment_expires_at,omitempty"`
	PocketUserID        string     `json:"pocket_user_id,omitempty"`
}
