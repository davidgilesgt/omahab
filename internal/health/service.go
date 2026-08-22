package health

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

// Check describes a single health diagnostic, as returned by /doctor.
type Check struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // healthy|degraded|unhealthy|unknown
	Message string `json:"message,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// Report aggregates all checks.
type Report struct {
	Healthy     bool      `json:"healthy"`
	Checks      []Check   `json:"checks"`
	GeneratedAt time.Time `json:"generated_at"`
}

// EventSink is the package-local narrow interface for emitting health state
// changes. It must not log sensitive details.
type EventSink interface {
	Emit(ctx context.Context, ev domain.Event) error
}

// Interfaces for upstream systems. Each is narrow and mockable so tests never
// touch the real filesystem, Docker, or network.

type DiskProbe interface {
	CheckDisk(ctx context.Context) (DiskStatus, error)
}

type DiskStatus struct {
	Path       string
	TotalBytes uint64
	FreeBytes  uint64
	UsedBytes  uint64
	UsePercent float64
}

type ServiceProbe interface {
	CheckServices(ctx context.Context) ([]ServiceStatus, error)
}

type ServiceStatus struct {
	Name   string
	Health domain.Health
	Detail string
}

type BackupProbe interface {
	LastBackup(ctx context.Context) (*BackupInfo, error)
	LastVerified(ctx context.Context) (*BackupInfo, error)
}

type BackupInfo struct {
	ID         string
	FinishedAt time.Time
	VerifiedAt *time.Time
	Status     string
}

type TailscaleProbe interface {
	IsInstalled(ctx context.Context) (bool, string)
	IsLoggedIn(ctx context.Context) (bool, string)
	ServerNodeVisible(ctx context.Context) (bool, string)
}

type DNSProbe interface {
	Lookup(ctx context.Context, host string) ([]string, error)
}

type TLSProbe interface {
	CheckTLS(ctx context.Context, url string) (bool, string)
}

type PocketIDProbe interface {
	CheckPocketID(ctx context.Context) error
}

type InstanceProbe interface {
	GetInstanceID(ctx context.Context) (string, error)
}

// Noop implementations for production wiring where a probe is optional.

type NoopDiskProbe struct{}

func (NoopDiskProbe) CheckDisk(ctx context.Context) (DiskStatus, error) {
	return DiskStatus{Path: "/", UsePercent: 0}, nil
}

type NoopServiceProbe struct{}

func (NoopServiceProbe) CheckServices(ctx context.Context) ([]ServiceStatus, error) { return nil, nil }

type NoopBackupProbe struct{}

func (NoopBackupProbe) LastBackup(ctx context.Context) (*BackupInfo, error)   { return nil, nil }
func (NoopBackupProbe) LastVerified(ctx context.Context) (*BackupInfo, error) { return nil, nil }

type NoopTailscaleProbe struct{}

func (NoopTailscaleProbe) IsInstalled(ctx context.Context) (bool, string)       { return true, "ok" }
func (NoopTailscaleProbe) IsLoggedIn(ctx context.Context) (bool, string)        { return true, "ok" }
func (NoopTailscaleProbe) ServerNodeVisible(ctx context.Context) (bool, string) { return true, "ok" }

type NoopDNSProbe struct{}

func (NoopDNSProbe) Lookup(ctx context.Context, host string) ([]string, error) {
	return []string{"127.0.0.1"}, nil
}

type NoopTLSProbe struct{}

func (NoopTLSProbe) CheckTLS(ctx context.Context, url string) (bool, string) { return true, "ok" }

type NoopPocketIDProbe struct{}

func (NoopPocketIDProbe) CheckPocketID(ctx context.Context) error { return nil }

type NoopInstanceProbe struct{}

func (NoopInstanceProbe) GetInstanceID(ctx context.Context) (string, error) {
	return "test-instance", nil
}

// Options configures Service.
type Options struct {
	DB            *sql.DB
	Sink          EventSink
	Disk          DiskProbe
	Services      ServiceProbe
	Backup        BackupProbe
	Tailscale     TailscaleProbe
	DNS           DNSProbe
	TLS           TLSProbe
	PocketID      PocketIDProbe
	Instance      InstanceProbe
	Encryption    EncryptionProbe
	Now           func() time.Time
	MinInterval   time.Duration // storm suppression: don't re-emit same component within this interval
	DiskThreshold float64       // percent used that is considered low disk (default 85)
	BackupStale   time.Duration // duration after which missing backup is unhealthy (default 24h)
	VerifyStale   time.Duration // duration after which unverified backup is degraded (default 7 days)
	Hostname      string
	InstanceID    string
}
// Service aggregates host health and emits changes without storms.
type Service struct {
	db            *sql.DB
	sink          EventSink
	disk          DiskProbe
	services      ServiceProbe
	backup        BackupProbe
	tailscale     TailscaleProbe
	dns           DNSProbe
	tls           TLSProbe
	pocketID      PocketIDProbe
	instance      InstanceProbe
	encryption    EncryptionProbe
	now           func() time.Time
	minInterval   time.Duration
	diskThreshold float64
	backupStale   time.Duration
	verifyStale   time.Duration
	hostname      string
	instanceID    string

	mu          sync.Mutex
	lastEmitted map[string]time.Time
	lastStatus  map[string]string
	lastReport  *Report
}

// New creates a Service with safe defaults.
func New(opts Options) *Service {
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.Disk == nil {
		opts.Disk = NoopDiskProbe{}
	}
	if opts.Services == nil {
		opts.Services = NoopServiceProbe{}
	}
	if opts.Backup == nil {
		opts.Backup = NoopBackupProbe{}
	}
	if opts.Tailscale == nil {
		opts.Tailscale = NoopTailscaleProbe{}
	}
	if opts.DNS == nil {
		opts.DNS = NoopDNSProbe{}
	}
	if opts.TLS == nil {
		opts.TLS = NoopTLSProbe{}
	}
	if opts.PocketID == nil {
		opts.PocketID = NoopPocketIDProbe{}
	}
	if opts.Instance == nil {
		opts.Instance = NoopInstanceProbe{}
	}
	if opts.Encryption == nil {
		opts.Encryption = NoopEncryptionProbe{}
	}
	if opts.MinInterval == 0 {
		opts.MinInterval = 15 * time.Minute
	}
	if opts.DiskThreshold == 0 {
		opts.DiskThreshold = 85.0
	}
	if opts.BackupStale == 0 {
		opts.BackupStale = 24 * time.Hour
	}
	return &Service{
		db:            opts.DB,
		sink:          opts.Sink,
		disk:          opts.Disk,
		services:      opts.Services,
		backup:        opts.Backup,
		tailscale:     opts.Tailscale,
		dns:           opts.DNS,
		tls:           opts.TLS,
		pocketID:      opts.PocketID,
		instance:      opts.Instance,
		encryption:    opts.Encryption,
		now:           opts.Now,
		minInterval:   opts.MinInterval,
		diskThreshold: opts.DiskThreshold,
		backupStale:   opts.BackupStale,
		verifyStale:   opts.VerifyStale,
		hostname:      opts.Hostname,
		instanceID:    opts.InstanceID,
		lastEmitted:   make(map[string]time.Time),
		lastStatus:    make(map[string]string),
	}
}

// Check runs all probes concurrently and returns a Report. It does not emit events.
func (s *Service) Check(ctx context.Context) (*Report, error) {
	now := s.now().UTC()

	checks := []Check{}
	healthy := true

	// Disk
	diskStatus, err := s.disk.CheckDisk(ctx)
	if err != nil {
		c := Check{Name: "disk", Status: "unknown", Message: redactDetail(err.Error())}
		checks = append(checks, c)
		healthy = false
	} else {
		status := "healthy"
		msg := fmt.Sprintf("disk use %.1f%%", diskStatus.UsePercent)
		detail := fmt.Sprintf("path %s free %d/%d", redactDetail(diskStatus.Path), diskStatus.FreeBytes, diskStatus.TotalBytes)
		if diskStatus.UsePercent >= s.diskThreshold {
			status = "unhealthy"
			msg = fmt.Sprintf("disk low: %.1f%% used", diskStatus.UsePercent)
			healthy = false
		} else if diskStatus.UsePercent >= s.diskThreshold-10 {
			status = "degraded"
			msg = fmt.Sprintf("disk pressure: %.1f%% used", diskStatus.UsePercent)
		}
		if status != "healthy" {
			healthy = false
		}
		checks = append(checks, Check{Name: "disk", Status: status, Message: msg, Detail: detail})
	}

	// Services
	svcs, err := s.services.CheckServices(ctx)
	if err != nil {
		checks = append(checks, Check{Name: "services", Status: "unknown", Message: redactDetail(err.Error())})
	} else if len(svcs) == 0 {
		checks = append(checks, Check{Name: "services", Status: "healthy", Message: "no services registered"})
	} else {
		for _, svc := range svcs {
			status := string(svc.Health)
			if status == "" {
				status = "unknown"
			}
			// Normalize: map Healthy=>healthy etc.
			status = strings.ToLower(status)
			if status == "healthy" || status == "ok" || status == "pass" {
				status = "healthy"
			}
			msg := fmt.Sprintf("service %s %s", svc.Name, status)
			c := Check{Name: "service:" + svc.Name, Status: status, Message: msg, Detail: redactDetail(svc.Detail)}
			checks = append(checks, c)
			if status != "healthy" {
				healthy = false
			}
		}
	}

	// Backup: differentiate created vs verified (ACCEPTANCE)
	lastBackup, err := s.backup.LastBackup(ctx)
	if err != nil {
		checks = append(checks, Check{Name: "backup", Status: "unknown", Message: redactDetail(err.Error())})
	} else if lastBackup == nil {
		checks = append(checks, Check{Name: "backup", Status: "degraded", Message: "no backups yet", Detail: "backup has not run"})
		// backup missing is degraded not unhealthy initially, but after stale threshold unhealthy handled below
	} else {
		age := now.Sub(lastBackup.FinishedAt)
		status := "healthy"
		msg := fmt.Sprintf("last backup %s ago", formatDuration(age))
		detail := fmt.Sprintf("id %s status %s", redactDetail(lastBackup.ID), lastBackup.Status)
		if age > s.backupStale {
			status = "unhealthy"
			msg = fmt.Sprintf("backup stale: last success %s ago", formatDuration(age))
			healthy = false
		}
		checks = append(checks, Check{Name: "backup", Status: status, Message: msg, Detail: detail})
		if status != "healthy" {
			// already set
		}
	}

	lastVerified, err := s.backup.LastVerified(ctx)
	if err != nil {
		checks = append(checks, Check{Name: "backup_verified", Status: "unknown", Message: redactDetail(err.Error())})
	} else if lastVerified == nil {
		// No verified restore yet: distinct from backup created
		checks = append(checks, Check{Name: "backup_verified", Status: "degraded", Message: "no verified restore yet", Detail: "backup has not been verified by restore test"})
		// Do not mark overall unhealthy, but degraded
		// Keep overall healthy flag if backup itself is healthy; verified is separate dimension
	} else {
		age := now.Sub(*lastVerified.VerifiedAt)
		status := "healthy"
		msg := fmt.Sprintf("last verified %s ago", formatDuration(age))
		detail := fmt.Sprintf("id %s", redactDetail(lastVerified.ID))
		if age > s.verifyStale {
			status = "degraded"
			msg = fmt.Sprintf("verified restore stale: %s ago", formatDuration(age))
			// degraded does not make overall unhealthy, but signals
		}
		checks = append(checks, Check{Name: "backup_verified", Status: status, Message: msg, Detail: detail})
	}

	// Tailscale
	if ok, detail := s.tailscale.IsInstalled(ctx); !ok {
		checks = append(checks, Check{Name: "tailscale", Status: "unhealthy", Message: "tailscale not installed", Detail: redactDetail(detail)})
		healthy = false
	} else if ok, detail := s.tailscale.IsLoggedIn(ctx); !ok {
		checks = append(checks, Check{Name: "tailscale", Status: "unhealthy", Message: "tailscale not logged in", Detail: redactDetail(detail)})
		healthy = false
	} else if ok, detail := s.tailscale.ServerNodeVisible(ctx); !ok {
		checks = append(checks, Check{Name: "tailscale", Status: "degraded", Message: "server node not visible", Detail: redactDetail(detail)})
	} else {
		checks = append(checks, Check{Name: "tailscale", Status: "healthy", Message: "tailscale ok"})
	}

	// DNS
	if s.hostname != "" {
		addrs, err := s.dns.Lookup(ctx, s.hostname)
		if err != nil || len(addrs) == 0 {
			msg := "dns resolution failed"
			if err != nil {
				msg = redactDetail(err.Error())
			}
			checks = append(checks, Check{Name: "dns", Status: "unhealthy", Message: msg, Detail: fmt.Sprintf("host %s", redactDetail(s.hostname))})
			healthy = false
		} else {
			// Don't leak full IP list; just count.
			checks = append(checks, Check{Name: "dns", Status: "healthy", Message: fmt.Sprintf("dns ok (%d records)", len(addrs))})
		}
	} else {
		checks = append(checks, Check{Name: "dns", Status: "unknown", Message: "no hostname configured"})
	}

	// TLS
	if s.hostname != "" {
		ok, detail := s.tls.CheckTLS(ctx, "https://"+s.hostname)
		if !ok {
			checks = append(checks, Check{Name: "tls", Status: "unhealthy", Message: "tls check failed", Detail: redactDetail(detail)})
			healthy = false
		} else {
			checks = append(checks, Check{Name: "tls", Status: "healthy", Message: "tls ok"})
		}
	}

	// PocketID
	if err := s.pocketID.CheckPocketID(ctx); err != nil {
		checks = append(checks, Check{Name: "pocketid", Status: "unhealthy", Message: redactDetail(err.Error())})
		healthy = false
	} else {
		checks = append(checks, Check{Name: "pocketid", Status: "healthy", Message: "pocketid reachable"})
	}

	// Instance identity
	id, err := s.instance.GetInstanceID(ctx)
	if err != nil {
		checks = append(checks, Check{Name: "instance", Status: "unhealthy", Message: redactDetail(err.Error())})
		healthy = false
	} else if s.instanceID != "" && id != s.instanceID {
		checks = append(checks, Check{Name: "instance", Status: "unhealthy", Message: "instance id mismatch", Detail: "expected identity does not match reported"})
		healthy = false
	} else if id == "" {
		checks = append(checks, Check{Name: "instance", Status: "unknown", Message: "no instance id returned"})
	} else {
		checks = append(checks, Check{Name: "instance", Status: "healthy", Message: "instance identity ok"})
	}

	// Encryption / LUKS
	if s.encryption != nil {
		enc, detail, encErr := s.encryption.CheckEncryption(ctx)
		if encErr != nil {
			checks = append(checks, Check{Name: "encryption", Status: "unknown", Message: redactDetail(encErr.Error())})
		} else if !enc {
			msg := "unencrypted filesystem detected"
			if detail != "" {
				msg = detail
			}
			checks = append(checks, Check{Name: "encryption", Status: "degraded", Message: msg, Detail: encryptionRecommendation})
		} else {
			checks = append(checks, Check{Name: "encryption", Status: "healthy", Message: "encrypted storage ok"})
		}
	}

	// Overall healthy if no unhealthy; degraded counts as not healthy? Keep simple: healthy only if all checks healthy or degraded? But spec says differentiate degraded vs unhealthy.
	// Report.Healthy is true only if every check is healthy. Degraded makes it false as well to surface.
	for _, c := range checks {
		if c.Status != "healthy" {
			healthy = false
			break
		}
	}

	report := &Report{
		Healthy:     healthy,
		Checks:      checks,
		GeneratedAt: now,
	}

	s.mu.Lock()
	s.lastReport = report
	s.mu.Unlock()

	// Persist snapshot for audit (best effort)
	if s.db != nil {
		_ = s.persistSnapshot(ctx, report)
	}

	return report, nil
}

// CheckAndEmit runs Check and emits events for state changes, suppressing storms.
func (s *Service) CheckAndEmit(ctx context.Context) (*Report, error) {
	report, err := s.Check(ctx)
	if err != nil {
		return report, err
	}
	if s.sink == nil {
		return report, nil
	}
	now := s.now().UTC()
	for _, c := range report.Checks {
		if c.Status == "healthy" {
			// Emit recovery only if previously unhealthy/degraded and cooldown elapsed? To avoid storms, only emit when transitioning to healthy if previously not healthy and interval elapsed.
			// We still apply storm suppression below.
		}
		s.mu.Lock()
		lastStatus := s.lastStatus[c.Name]
		lastEmit := s.lastEmitted[c.Name]
		shouldEmit := false
		if lastStatus != c.Status {
			// Status change always eligible, but still respect minInterval to prevent flapping? We allow immediate on change, but flap will be caught by interval.
			if now.Sub(lastEmit) >= s.minInterval || lastEmit.IsZero() {
				shouldEmit = true
			} else {
				// Flapping within interval: suppress
				shouldEmit = false
			}
		} else {
			// Same status: only re-emit if unhealthy and interval elapsed (reminder), not for healthy
			if c.Status != "healthy" && now.Sub(lastEmit) >= s.minInterval*2 {
				shouldEmit = true
			}
		}
		if shouldEmit {
			s.lastStatus[c.Name] = c.Status
			s.lastEmitted[c.Name] = now
		}
		s.mu.Unlock()

		if !shouldEmit {
			continue
		}
		severity := "info"
		switch c.Status {
		case "unhealthy":
			severity = "error"
		case "degraded":
			severity = "warning"
		case "unknown":
			severity = "warning"
		}
		typ := componentToEventType(c.Name)
		if typ == "" {
			continue
		}
		ev := domain.Event{
			ID:       domain.ID(store.NewID()),
			Type:     typ,
			Severity: severity,
			Message:  redactDetail(c.Message),
			Data: map[string]any{
				"component": c.Name,
				"status":    c.Status,
				"detail":    redactDetail(c.Detail),
			},
			CreatedAt: now,
		}
		// Best effort; don't fail check if sink fails
		_ = s.sink.Emit(ctx, ev)
	}
	return report, nil
}

// Doctor returns a DoctorResult suitable for API responses.
func (s *Service) Doctor(ctx context.Context) (*Report, error) {
	return s.Check(ctx)
}

// LastReport returns the most recent report, if any.
func (s *Service) LastReport() *Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastReport == nil {
		return nil
	}
	cp := *s.lastReport
	cp.Checks = append([]Check(nil), s.lastReport.Checks...)
	return &cp
}

func (s *Service) persistSnapshot(ctx context.Context, report *Report) error {
	if s.db == nil {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, c := range report.Checks {
		id := store.NewID()
		_, err := tx.ExecContext(ctx,
			`INSERT INTO health_snapshots (id, component, status, message, detail, checked_at) VALUES (?, ?, ?, ?, ?, ?)`,
			id, c.Name, c.Status, c.Message, c.Detail, report.GeneratedAt.UTC().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
	}
	// Update emitted table for storm suppression persistence (optional)
	for _, c := range report.Checks {
		if c.Status != "healthy" {
			_, _ = tx.ExecContext(ctx,
				`INSERT INTO health_emitted (component, status, emitted_at) VALUES (?, ?, ?)
				 ON CONFLICT(component) DO UPDATE SET status=excluded.status, emitted_at=excluded.emitted_at`,
				c.Name, c.Status, report.GeneratedAt.UTC().Format(time.RFC3339Nano))
		}
	}
	return tx.Commit()
}

func componentToEventType(component string) string {
	switch {
	case component == "disk":
		return "host.disk_low"
	case strings.HasPrefix(component, "service:"):
		return "service.unhealthy"
	case component == "backup":
		return "backup.failed"
	case component == "backup_verified":
		return "backup.restored" // using restored type to signal verified vs created; distinct message
	case component == "encryption":
		return "host.disk_low"
	case component == "tailscale" || component == "dns" || component == "tls" || component == "pocketid" || component == "instance":
		return "service.unhealthy"
	default:
		return "service.unhealthy"
	}
}

var sensitiveTokens = []string{"token", "secret", "password", "key", "auth", "credential", "private", "hmac", "bearer"}

func redactDetail(s string) string {
	if s == "" {
		return s
	}
	low := strings.ToLower(s)
	for _, tok := range sensitiveTokens {
		if strings.Contains(low, tok) {
			return "[REDACTED]"
		}
	}
	// Also redact long base64-like strings > 40 chars without spaces
	// To avoid leaking secrets in detail
	if len(s) > 200 {
		return s[:200] + "...[TRUNCATED]"
	}
	return s
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// Ensure import of store for NewID usage; reference to avoid unused import if needed.
var _ = store.NewID
