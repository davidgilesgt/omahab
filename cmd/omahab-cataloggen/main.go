// Command omahab-cataloggen converts the curated release catalog
// (deploy/catalog) into the runtime application catalog consumed by omahabd.
//
// Inputs:
//
//	-catalog     curated catalog.json (bundle metadata, digest placeholders)
//	-compose-dir directory holding the compose/ templates referenced by it
//	-digests     JSON object mapping image key -> resolved sha256 digest
//	-out         output path for the runtime catalog
//
// Every ${VAR:?...} image placeholder must resolve to a pinned digest from
// the signed release manifest; the generator fails closed on any missing,
// malformed, or placeholder digest, and the emitted bundles are validated
// through apps.NewCatalog before the file is written.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/domain"
)

type curatedBundle struct {
	Name                   string            `json:"name"`
	DisplayName            string            `json:"displayName"`
	ComposeFile            string            `json:"composeFile"`
	SupportedArchitectures []string          `json:"supportedArchitectures"`
	Images                 map[string]string `json:"images"`
	PipelineImage          string            `json:"pipelineImage,omitempty"`
	PipelineImageKey       string            `json:"pipelineImageKey,omitempty"`
	Resources              struct {
		MemoryRecommendedMiB int `json:"memoryRecommendedMiB"`
	} `json:"resources"`
	Persistence []struct {
		Volume    string `json:"volume"`
		MountPath string `json:"mountPath"`
	} `json:"persistence"`
	HealthCheck struct {
		Endpoint string   `json:"endpoint"`
		Test     []string `json:"test"`
	} `json:"healthCheck"`
	Backup struct {
		PreHooks  []string `json:"preHooks"`
		PostHooks []string `json:"postHooks"`
	} `json:"backup"`
	Restore struct {
		Hooks []string `json:"hooks"`
	} `json:"restore"`
	OIDC struct {
		Supported bool `json:"supported"`
	} `json:"oidc"`
	Exposure struct {
		Default      string   `json:"default"`
		Allowed      []string `json:"allowed"`
		CaddyRoute   string   `json:"caddyRoute"`
		InternalPort int      `json:"internalPort"`
	} `json:"exposure"`
	EnabledByDefault bool     `json:"enabledByDefault"`
	Dependencies     []string `json:"dependencies"`
	Runtime          string   `json:"runtime,omitempty"`
	Units            []string `json:"units,omitempty"`
	Secrets          struct {
		SecretFiles []struct {
			Source string `json:"source"`
			Target string `json:"target"`
			Env    string `json:"env"`
		} `json:"secretFiles"`
	} `json:"secrets"`
}

type curatedDoc struct {
	Bundles []curatedBundle `json:"bundles"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "omahab-cataloggen: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("omahab-cataloggen", flag.ContinueOnError)
	catalogPath := fs.String("catalog", "deploy/catalog/catalog.json", "curated catalog JSON")
	composeDir := fs.String("compose-dir", "deploy/catalog", "directory holding compose/ templates")
	digestsPath := fs.String("digests", "", "JSON object mapping image key to resolved sha256 digest (required)")
	outPath := fs.String("out", "apps-catalog.json", "output runtime catalog path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *digestsPath == "" {
		return fmt.Errorf("-digests is required")
	}

	digests, err := loadDigests(*digestsPath)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(*catalogPath)
	if err != nil {
		return fmt.Errorf("read catalog: %w", err)
	}
	var doc curatedDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse catalog: %w", err)
	}
	if len(doc.Bundles) == 0 {
		return fmt.Errorf("catalog contains no bundles")
	}

	var bundles []apps.Bundle
	enabledRequired := map[string]bool{}
	for _, cb := range doc.Bundles {
		if cb.EnabledByDefault {
			// hermes is skipped-with-warning above while its image is
			// unpublished; see that block for details.
			if cb.Name == "hermes" && cb.Runtime != apps.RuntimeSystemd {
				continue
			}
			enabledRequired[cb.Name] = false
		}
	}
	for _, cb := range doc.Bundles {
		b, err := convert(cb, *composeDir, digests)
		if err != nil {
			if strings.Contains(err.Error(), "no resolved digest") {
				// Native (systemd) bundles need no digest. Hermes is the one
				// digest-pinned compose bundle whose image is not yet
				// published; it is skipped with a warning until the image
				// exists (installing it then returns "unknown bundle").
				if cb.EnabledByDefault && cb.Runtime != apps.RuntimeSystemd && cb.Name != "hermes" {
					return fmt.Errorf("bundle %q: %w", cb.Name, err)
				}
				fmt.Fprintf(os.Stderr, "warning: skip bundle %q: %v\n", cb.Name, err)
				continue
			}
			return fmt.Errorf("bundle %q: %w", cb.Name, err)
		}
		if _, ok := enabledRequired[b.ID]; ok {
			enabledRequired[b.ID] = true
		}
		bundles = append(bundles, b)
	}
	for id, present := range enabledRequired {
		if !present {
			return fmt.Errorf("generated catalog missing required bundle %q", id)
		}
	}
	if _, err := apps.NewCatalog(bundles...); err != nil {
		return fmt.Errorf("generated catalog rejected by validation: %w", err)
	}

	out, err := json.MarshalIndent(struct {
		Bundles []apps.Bundle `json:"bundles"`
	}{Bundles: bundles}, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if err := os.MkdirAll(filepath.Dir(*outPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(*outPath, out, 0o644)
}

func loadDigests(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read digests: %w", err)
	}
	var digests map[string]string
	if err := json.Unmarshal(raw, &digests); err != nil {
		return nil, fmt.Errorf("parse digests: %w", err)
	}
	for key, digest := range digests {
		if !apps.ValidDigest(digest) {
			return nil, fmt.Errorf("digests[%q] is not a pinned sha256 digest (got %q)", key, digest)
		}
	}
	return digests, nil
}

func convert(cb curatedBundle, composeDir string, digests map[string]string) (apps.Bundle, error) {
	if len(cb.Images) == 0 {
		return apps.Bundle{}, fmt.Errorf("no images declared")
	}
	compose := ""
	if cb.Runtime != apps.RuntimeSystemd {
		composeRaw, err := os.ReadFile(filepath.Join(composeDir, cb.ComposeFile))
		if err != nil {
			return apps.Bundle{}, fmt.Errorf("read compose: %w", err)
		}
		compose = string(composeRaw)
	}

	primary := primaryImageKey(cb)
	primaryRepo := ""
	digest := ""
	if cb.Runtime != apps.RuntimeSystemd {
		keys := make([]string, 0, len(cb.Images))
		for key := range cb.Images {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			ref := cb.Images[key]
			repo, variable, ok := splitImageRef(ref)
			if !ok {
				return apps.Bundle{}, fmt.Errorf("image %q must be repo@sha256:${VAR} with a digest placeholder", ref)
			}
			keyDigest, ok := digests[key]
			if !ok {
				return apps.Bundle{}, fmt.Errorf("no resolved digest for image key %q", key)
			}
			// The compose template may phrase the placeholder's error message
			// differently from the catalog entry; match on the variable name.
			pattern := regexp.MustCompile(regexp.QuoteMeta(repo+"@sha256:") + `\$\{` + regexp.QuoteMeta(variable) + `:[^}]*\}`)
			if pattern.FindString(compose) == "" {
				return apps.Bundle{}, fmt.Errorf("compose template does not reference image %q", ref)
			}
			replacement := repo + "@" + keyDigest
			if key == primary {
				primaryRepo = repo
				replacement = "{{.Image}}@{{.Digest}}"
			}
			compose = pattern.ReplaceAllString(compose, replacement)
		}
		if primaryRepo == "" {
			return apps.Bundle{}, fmt.Errorf("primary image key %q not present in images", primary)
		}
		digest = digests[primary]
	}
	b := apps.Bundle{
		ID:            cb.Name,
		Name:          cb.DisplayName,
		Image:         primaryRepo,
		Digest:        digest,
		Architectures: cb.SupportedArchitectures,
		Compose:       compose,
	}
	for _, p := range cb.Persistence {
		// Host-path binds are carried by the compose file itself; only named
		// volumes become declarative data volumes.
		if p.Volume == "" {
			continue
		}
		b.Data = append(b.Data, apps.DataVolume{Name: p.Volume, Path: p.MountPath})
	}
	b.DefaultExposure = exposureValue(cb.Exposure.Default)
	b.MaxExposure = maxAllowed(cb.Exposure.Allowed)
	b.OIDC = apps.OIDCConfig{Supported: cb.OIDC.Supported}
	b.Resources = apps.ResourceGuidance{MemoryMB: cb.Resources.MemoryRecommendedMiB}
	b.HealthCheck = healthCheck(cb, primary)
	b.Backup = apps.BackupHooks{
		PreBackup:   hookArgv(cb.Backup.PreHooks, cb.Name),
		PostRestore: hookArgv(cb.Restore.Hooks, cb.Name),
	}
	b.Runtime = cb.Runtime
	b.Units = append([]string(nil), cb.Units...)
	b.Default = cb.EnabledByDefault
	b.Port = cb.Exposure.InternalPort
	route := strings.TrimSpace(cb.Exposure.CaddyRoute)
	if route == "edge" {
		route = ""
	} else {
		if route != "" && cb.Runtime != apps.RuntimeSystemd && (cb.Exposure.InternalPort < 1 || cb.Exposure.InternalPort > 65535) {
			return apps.Bundle{}, fmt.Errorf("caddyRoute %q requires internalPort in 1..65535, got %d", route, cb.Exposure.InternalPort)
		}
		route = strings.TrimSuffix(route, ".{{.Domain}}")
	}
	b.Route = route
	if len(cb.Dependencies) > 0 {
		b.Dependencies = append([]string(nil), cb.Dependencies...)
	}
	for _, sf := range cb.Secrets.SecretFiles {
		if s := strings.TrimSpace(sf.Source); s != "" {
			b.SecretSources = append(b.SecretSources, s)
		}
	}
	pipelineImage := strings.TrimSpace(cb.PipelineImage)
	pipelineKey := strings.TrimSpace(cb.PipelineImageKey)
	if pipelineImage != "" && pipelineKey != "" {
		return apps.Bundle{}, fmt.Errorf("bundle %q: pipelineImage and pipelineImageKey are mutually exclusive", cb.Name)
	}
	if pipelineKey != "" {
		digest, ok := digests[pipelineKey]
		if !ok {
			return apps.Bundle{}, fmt.Errorf("no resolved digest for pipeline image key %q", pipelineKey)
		}
		if !apps.ValidDigest(digest) {
			return apps.Bundle{}, fmt.Errorf("pipeline image digest %q is not a pinned sha256 digest", digest)
		}
		// pipelineImageKey expects a known repository; for podman use quay.io/podman/stable
		repo := ""
		switch pipelineKey {
		case "podman":
			repo = "quay.io/podman/stable"
		default:
			return apps.Bundle{}, fmt.Errorf("unknown pipeline image key %q", pipelineKey)
		}
		b.PipelineImage = repo + "@" + digest
	} else if pipelineImage != "" {
		repo, variable, ok := splitImageRef(pipelineImage)
		if !ok {
			return apps.Bundle{}, fmt.Errorf("pipelineImage %q must be repo@sha256:${VAR} with a digest placeholder", pipelineImage)
		}
		// variable corresponds to digest key with _DIGEST suffix; try to map: e.g. PODMAN_DIGEST -> podman
		key := ""
		// First try exact variable lowercased without _DIGEST suffix
		lowerVar := strings.ToLower(variable)
		if strings.HasSuffix(lowerVar, "_digest") {
			candidate := strings.TrimSuffix(lowerVar, "_digest")
			candidate = strings.ReplaceAll(candidate, "_", "-")
			if _, ok := digests[candidate]; ok {
				key = candidate
			}
		}
		if key == "" {
			// fallback: search digests keys whose variable matches case-insensitively
			for k := range digests {
				if strings.EqualFold(k+"_digest", variable) || strings.EqualFold(strings.ReplaceAll(k, "-", "_")+"_digest", variable) {
					key = k
					break
				}
			}
		}
		if key == "" {
			return apps.Bundle{}, fmt.Errorf("no resolved digest for pipeline image variable %q", variable)
		}
		digest, ok := digests[key]
		if !ok {
			return apps.Bundle{}, fmt.Errorf("no resolved digest for pipeline image key %q", key)
		}
		b.PipelineImage = repo + "@" + digest
	}
	return b, nil
}

// primaryImageKey picks the bundle's primary service: the image key matching
// the health endpoint host, then the bundle name, then the shortest key
// sharing the bundle name as a prefix.
func primaryImageKey(cb curatedBundle) string {
	if host := endpointHost(cb.HealthCheck.Endpoint); host != "" {
		if _, ok := cb.Images[host]; ok {
			return host
		}
	}
	if _, ok := cb.Images[cb.Name]; ok {
		return cb.Name
	}
	best := ""
	for key := range cb.Images {
		if strings.HasPrefix(key, cb.Name) && (best == "" || len(key) < len(best)) {
			best = key
		}
	}
	if best != "" {
		return best
	}
	for key := range cb.Images {
		return key
	}
	return ""
}

func endpointHost(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func splitImageRef(ref string) (repo, variable string, ok bool) {
	at := strings.Index(ref, "@sha256:")
	if at < 0 {
		return "", "", false
	}
	repo, rest := ref[:at], ref[at+len("@sha256:"):]
	if !strings.HasPrefix(rest, "${") || !strings.Contains(rest, "}") {
		return "", "", false
	}
	inner := strings.TrimSuffix(rest[2:], "}")
	if name, _, found := strings.Cut(inner, ":"); found {
		inner = name
	}
	if inner == "" {
		return "", "", false
	}
	return repo, inner, true
}

func exposureValue(v string) domain.Exposure {
	switch domain.Exposure(v) {
	case domain.ExposurePrivate, domain.ExposureShared, domain.ExposurePublic:
		return domain.Exposure(v)
	default:
		return domain.ExposurePrivate
	}
}

func maxAllowed(allowed []string) domain.Exposure {
	max := domain.ExposurePrivate
	for _, a := range allowed {
		switch domain.Exposure(a) {
		case domain.ExposurePublic:
			max = domain.ExposurePublic
		case domain.ExposureShared:
			if max != domain.ExposurePublic {
				max = domain.ExposureShared
			}
		}
	}
	return max
}

func healthCheck(cb curatedBundle, primary string) apps.HealthCheck {
	if test := cb.HealthCheck.Test; len(test) > 0 {
		if test[0] == "CMD-SHELL" && len(test) > 1 {
			return apps.HealthCheck{Kind: apps.CheckCommand, Service: primary, Command: []string{"sh", "-c", test[1]}}
		}
		if test[0] == "CMD" && len(test) > 1 {
			return apps.HealthCheck{Kind: apps.CheckCommand, Service: primary, Command: test[1:]}
		}
	}
	if endpoint := cb.HealthCheck.Endpoint; endpoint != "" {
		if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
			hc := apps.HealthCheck{Kind: apps.CheckHTTP, Path: u.Path}
			if hc.Path == "" {
				hc.Path = "/"
			}
			if port := u.Port(); port != "" {
				fmt.Sscanf(port, "%d", &hc.Port)
			}
			return hc
		}
	}
	return apps.HealthCheck{Kind: apps.CheckNone}
}

// hookArgv joins the curated shell hook lines into one argv vector. The
// {{.ComposeFile}} template variable becomes the app's rendered compose path
// under /srv/omahab/apps.
func hookArgv(lines []string, bundle string) []string {
	if len(lines) == 0 {
		return nil
	}
	joined := strings.Join(lines, " && ")
	joined = strings.ReplaceAll(joined, "{{.ComposeFile}}", "/srv/omahab/apps/"+bundle+"/compose.yaml")
	return []string{"/bin/sh", "-c", joined}
}
