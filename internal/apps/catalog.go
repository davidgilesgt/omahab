package apps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

// CheckKind selects how an application's health is observed.
type CheckKind string

const (
	CheckNone    CheckKind = "none"
	CheckHTTP    CheckKind = "http"
	CheckCommand CheckKind = "command"
)

// supportedArchitectures mirrors the supported Debian 13 / Ubuntu 26.04 hosts (amd64 and
// arm64, DESIGN.md §5.1). Bundles must declare a subset.
var supportedArchitectures = map[string]bool{
	"amd64": true,
	"arm64": true,
}

var (
	slugRe      = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)
	hostnameRe  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
	imageRe     = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9._-]+)*$`)
	digestRe    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	volumeName  = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]*[a-z0-9])?$`)
	imageLineRe = regexp.MustCompile(`(?m)^[ \t-]*image:[ \t]*(\S+)`)
	routeRe     = regexp.MustCompile(`^[a-z0-9-]*$`)
)

// ValidDigest reports whether d is a canonical pinned sha256 digest.
// Mutable tags (":latest", ":v1.2", floating aliases) are never accepted
// anywhere in this package.
func ValidDigest(d string) bool { return digestRe.MatchString(d) }

// DataVolume declares one piece of persistent application data. Names map to
// the Compose volume, Path to the mount point inside the service.
type DataVolume struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// BackupHooks names the database-safe hooks a bundle supplies. Values are
// argv vectors (typically ["/bin/sh","-c","..."]) because hooks execute
// without a shell; copying live database files is not a valid backup, so
// bundles with a database must declare pre_backup and post_restore.
type BackupHooks struct {
	PreBackup   []string `json:"pre_backup,omitempty"`
	PostRestore []string `json:"post_restore,omitempty"`
}

// OIDCConfig records whether the application supports native OIDC.
type OIDCConfig struct {
	Supported bool `json:"supported"`
}

// ResourceGuidance carries scheduling guidance for the bundle.
type ResourceGuidance struct {
	MemoryMB int `json:"memory_mb,omitempty"`
}

// HealthCheck describes how health is observed for a bundle. HTTP checks
// target the loopback-published port from the Compose definition (no direct
// external publication); command checks run inside a Compose service.
type HealthCheck struct {
	Kind     CheckKind `json:"kind"`
	Path     string    `json:"path,omitempty"`
	Port     int       `json:"port,omitempty"`
	Service  string    `json:"service,omitempty"`
	Command  []string  `json:"command,omitempty"`
	Interval string    `json:"interval,omitempty"`
	Timeout  string    `json:"timeout,omitempty"`
}

func (h HealthCheck) timeout() time.Duration { return parseDurationOrDefault(h.Timeout, 5*time.Second) }

func parseDurationOrDefault(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

// Bundle is one curated platform application entry (DESIGN.md §6.1): a
// pinned digest, a Compose definition template, supported architectures,
// health and exposure capability, persistent-data declarations, and backup
// hooks. Compose is a text/template whose available fields are Image,
// Digest, and Name; the rendered definition must reference every image by
// digest and must pin the bundle's own image to the requested digest.
type Bundle struct {
	ID              string           `json:"id"`
	Name            string           `json:"name"`
	Image           string           `json:"image"`
	Digest          string           `json:"digest"`
	Architectures   []string         `json:"architectures"`
	Compose         string           `json:"compose"`
	Port            int              `json:"port,omitempty"`
	DefaultExposure domain.Exposure  `json:"default_exposure,omitempty"`
	MaxExposure     domain.Exposure  `json:"max_exposure,omitempty"`
	HealthCheck     HealthCheck      `json:"health_check"`
	Data            []DataVolume     `json:"data,omitempty"`
	Backup          BackupHooks      `json:"backup,omitempty"`
	OIDC            OIDCConfig       `json:"oidc,omitempty"`
	Resources       ResourceGuidance `json:"resources,omitempty"`
	Default         bool             `json:"default"`
	Route           string           `json:"route"`
	Dependencies    []string         `json:"dependencies,omitempty"`
	SecretSources   []string         `json:"secret_sources,omitempty"`
}

// exposureRank orders exposure so requests can be checked against a bundle's
// capability ceiling. Empty ranks as private.
func exposureRank(e domain.Exposure) int {
	switch e {
	case "", domain.ExposurePrivate:
		return 0
	case domain.ExposureShared:
		return 1
	case domain.ExposurePublic:
		return 2
	default:
		return -1
	}
}

func validSlug(s string) bool {
	return len(s) <= 63 && slugRe.MatchString(s)
}

// validate normalizes a bundle and enforces the catalog contract. It returns
// a copy; the receiver is never mutated.
func (b Bundle) validate() (Bundle, error) {
	var problems []string

	if !validSlug(b.ID) || len(b.ID) < 2 {
		problems = append(problems, fmt.Sprintf("id %q must be 2-63 chars of [a-z0-9-], starting with a letter and ending alphanumeric", b.ID))
	}
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" {
		problems = append(problems, "name is required")
	}
	if b.Image == "" || strings.Contains(b.Image, "@") || !imageRe.MatchString(b.Image) {
		problems = append(problems, fmt.Sprintf("image %q must be a tagless lowercase repository reference (digests belong in the digest field)", b.Image))
	} else if idx := strings.LastIndexByte(b.Image, '/'); idx >= 0 && strings.ContainsRune(b.Image[idx:], ':') {
		problems = append(problems, fmt.Sprintf("image %q carries a mutable tag", b.Image))
	}
	if !ValidDigest(b.Digest) {
		problems = append(problems, fmt.Sprintf("digest %q must be a pinned sha256:<64 lowercase hex> digest", b.Digest))
	}
	if b.Port < 0 || b.Port > 65535 {
		problems = append(problems, fmt.Sprintf("port %d out of range", b.Port))
	}

	if len(b.Architectures) == 0 {
		problems = append(problems, "architectures must list at least one of amd64, arm64")
	}
	seenArch := map[string]bool{}
	for _, a := range b.Architectures {
		if !supportedArchitectures[a] {
			problems = append(problems, fmt.Sprintf("architecture %q is not supported (want amd64 and/or arm64)", a))
		}
		if seenArch[a] {
			problems = append(problems, fmt.Sprintf("architecture %q listed twice", a))
		}
		seenArch[a] = true
	}

	if b.DefaultExposure == "" {
		b.DefaultExposure = domain.ExposurePrivate
	}
	if b.MaxExposure == "" {
		b.MaxExposure = domain.ExposurePrivate
	}
	if exposureRank(b.DefaultExposure) < 0 {
		problems = append(problems, fmt.Sprintf("default_exposure %q is not private, shared, or public", b.DefaultExposure))
	}
	if exposureRank(b.MaxExposure) < 0 {
		problems = append(problems, fmt.Sprintf("max_exposure %q is not private, shared, or public", b.MaxExposure))
	}
	if exposureRank(b.DefaultExposure) > exposureRank(b.MaxExposure) {
		problems = append(problems, fmt.Sprintf("default_exposure %q exceeds max_exposure %q", b.DefaultExposure, b.MaxExposure))
	}

	var hcProblems []string
	b.HealthCheck, hcProblems = validateHealthCheck(b.HealthCheck, b.Port)
	problems = append(problems, hcProblems...)

	seenVol := map[string]bool{}
	for _, v := range b.Data {
		if !volumeName.MatchString(v.Name) {
			problems = append(problems, fmt.Sprintf("data volume name %q must be [a-z0-9_-]", v.Name))
		}
		if !strings.HasPrefix(v.Path, "/") {
			problems = append(problems, fmt.Sprintf("data volume %q path %q must be absolute", v.Name, v.Path))
		}
		if seenVol[v.Name] {
			problems = append(problems, fmt.Sprintf("data volume %q declared twice", v.Name))
		}
		seenVol[v.Name] = true
	}

	for name, argv := range map[string][]string{"pre_backup": b.Backup.PreBackup, "post_restore": b.Backup.PostRestore} {
		for _, arg := range argv {
			if strings.TrimSpace(arg) == "" {
				problems = append(problems, fmt.Sprintf("backup hook %s contains an empty argument", name))
			}
		}
	}
	if b.Resources.MemoryMB < 0 {
		problems = append(problems, "resources.memory_mb must not be negative")
	}
	if !routeRe.MatchString(b.Route) {
		problems = append(problems, fmt.Sprintf("route %q must match ^[a-z0-9-]*$", b.Route))
	}
	if len(b.Route) > 63 {
		problems = append(problems, fmt.Sprintf("route %q must be at most 63 chars", b.Route))
	}
	seenDep := map[string]bool{}
	for _, dep := range b.Dependencies {
		if !validSlug(dep) || len(dep) < 2 {
			problems = append(problems, fmt.Sprintf("dependency %q must be 2-63 chars of [a-z0-9-], starting with a letter and ending alphanumeric", dep))
		}
		if dep == b.ID {
			problems = append(problems, fmt.Sprintf("dependency %q cannot be self", dep))
		}
		if seenDep[dep] {
			problems = append(problems, fmt.Sprintf("dependency %q listed twice", dep))
		}
		seenDep[dep] = true
	}
	seenSecret := map[string]bool{}
	for _, src := range b.SecretSources {
		if strings.TrimSpace(src) == "" {
			problems = append(problems, "secret source must not be empty")
		}
		if seenSecret[src] {
			problems = append(problems, fmt.Sprintf("secret source %q listed twice", src))
		}
		seenSecret[src] = true
	}
	if len(problems) > 0 {
		return Bundle{}, &ValidationError{Problems: problems}
	}
	return b, nil
}

func validateHealthCheck(h HealthCheck, bundlePort int) (HealthCheck, []string) {
	var problems []string
	switch h.Kind {
	case "", CheckNone:
		h.Kind = CheckNone
	case CheckHTTP:
		if h.Path == "" {
			h.Path = "/"
		}
		if !strings.HasPrefix(h.Path, "/") {
			problems = append(problems, fmt.Sprintf("health check path %q must start with /", h.Path))
		}
		if h.Port == 0 {
			h.Port = bundlePort
		}
		if h.Port <= 0 || h.Port > 65535 {
			problems = append(problems, "health check kind http requires a port (check port or bundle port)")
		}
	case CheckCommand:
		if h.Service == "" || len(h.Command) == 0 {
			problems = append(problems, "health check kind command requires service and command")
		}
	default:
		problems = append(problems, fmt.Sprintf("health check kind %q must be none, http, or command", h.Kind))
	}
	if h.Interval != "" {
		if _, err := time.ParseDuration(h.Interval); err != nil {
			problems = append(problems, fmt.Sprintf("health check interval %q is not a duration", h.Interval))
		}
	}
	if h.Timeout != "" {
		if _, err := time.ParseDuration(h.Timeout); err != nil {
			problems = append(problems, fmt.Sprintf("health check timeout %q is not a duration", h.Timeout))
		}
	}
	return h, problems
}

// renderCompose renders the bundle's Compose template for a specific pinned
// digest and verifies that every image reference is digest-pinned and that
// the bundle's own image is pinned to exactly that digest.
func renderCompose(b Bundle, digest string) (string, error) {
	if !ValidDigest(digest) {
		return "", invalid("digest %q must be a pinned sha256 digest; mutable tags are not accepted", digest)
	}
	tmpl, err := template.New("compose").Parse(b.Compose)
	if err != nil {
		return "", invalid("bundle %q compose template: %v", b.ID, err)
	}
	var buf bytes.Buffer
	data := struct {
		Image  string
		Digest string
		Name   string
	}{b.Image, digest, b.Name}
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", invalid("bundle %q compose render: %v", b.ID, err)
	}
	rendered := buf.String()
	refs, err := imageRefs(rendered)
	if err != nil {
		return "", fmt.Errorf("bundle %q: %w", b.ID, err)
	}
	want := b.Image + "@" + digest
	for _, ref := range refs {
		if ref == want {
			return rendered, nil
		}
	}
	return "", invalid("bundle %q compose does not pin %s to digest %s", b.ID, b.Image, digest)
}

// imageRefs extracts the image references from a rendered Compose document
// and rejects any that are not digest-pinned.
func imageRefs(compose string) ([]string, error) {
	var refs []string
	for _, m := range imageLineRe.FindAllStringSubmatch(compose, -1) {
		ref := strings.Trim(m[1], `"'`)
		at := strings.IndexByte(ref, '@')
		if at < 0 || !digestRe.MatchString(ref[at+1:]) {
			return nil, invalid("image %q is not pinned by sha256 digest; mutable tags are not accepted", ref)
		}
		refs = append(refs, ref)
	}
	if len(refs) == 0 {
		return nil, invalid("compose defines no image references")
	}
	return refs, nil
}

// Catalog is the validated set of curated bundles. It is immutable once
// constructed; a new catalog is built when a curated file changes.
type Catalog struct {
	byID  map[string]Bundle
	order []string
}

// NewCatalog validates every bundle and indexes it by ID.
func NewCatalog(bundles ...Bundle) (*Catalog, error) {
	c := &Catalog{byID: make(map[string]Bundle, len(bundles))}
	for _, b := range bundles {
		nb, err := b.validate()
		if err != nil {
			return nil, fmt.Errorf("bundle %q: %w", b.ID, err)
		}
		if _, dup := c.byID[nb.ID]; dup {
			return nil, invalid("duplicate bundle id %q", nb.ID)
		}
		if _, err := renderCompose(nb, nb.Digest); err != nil {
			return nil, fmt.Errorf("bundle %q: %w", nb.ID, err)
		}
		c.byID[nb.ID] = nb
		c.order = append(c.order, nb.ID)
	}
	for _, b := range c.byID {
		for _, dep := range b.Dependencies {
			if _, ok := c.byID[dep]; !ok {
				return nil, invalid("bundle %q depends on unknown bundle %q", b.ID, dep)
			}
		}
	}
	return c, nil
}

// ParseCatalog decodes a curated catalog document. Unknown fields are
// rejected so catalog typos surface at load time rather than at deploy time.
func ParseCatalog(r io.Reader) (*Catalog, error) {
	var doc struct {
		Bundles []Bundle `json:"bundles"`
	}
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, invalid("catalog JSON: %v", err)
	}
	return NewCatalog(doc.Bundles...)
}

// LoadCatalogFile reads and validates a runtime catalog document. A missing
// file returns an error wrapping fs.ErrNotExist so callers can distinguish
// "no release catalog shipped" from a corrupt one.
func LoadCatalogFile(path string) (*Catalog, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseCatalog(f)
}

// Get returns the bundle with the given ID.
func (c *Catalog) Get(id string) (Bundle, bool) {
	b, ok := c.byID[id]
	return b, ok
}

// IDs lists bundle IDs in catalog order.
func (c *Catalog) IDs() []string {
	out := make([]string, len(c.order))
	copy(out, c.order)
	return out
}

// Bundles returns every bundle in catalog order.
func (c *Catalog) Bundles() []Bundle {
	out := make([]Bundle, 0, len(c.order))
	for _, id := range c.order {
		out = append(out, c.byID[id])
	}
	return out
}
