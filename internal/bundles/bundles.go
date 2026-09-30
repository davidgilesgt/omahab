// Package bundles manages the two sources of platform bundles (DESIGN.md
// §6.1): the curated set shipped with the release (bundles/<id>/bundle.json
// assembled into the runtime catalog by the omahab-catalog package and
// imported as NixOS modules from bundles/<id>/module.nix) and custom
// bundles living in a git repo at /var/lib/omahab/bundles with the same
// per-bundle layout.
//
// Loader contract: every bundle JSON file holds exactly one catalog Bundle
// object (same schema as the former deploy/catalog/catalog.json entries).
// "source" is loader-assigned — "catalog" for curated entries, "custom"
// for repo entries — and must never appear in a bundle.json file. Custom
// bundle files are read at HEAD (git show), never from the dirty worktree,
// so only committed state is validated, built, and served.
package bundles

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/omahab/omahab/internal/apps"
)

// ValidationError reports field-level problems; it aliases the apps
// validation error so API callers keep one error shape.
type ValidationError = apps.ValidationError

// RepoPath is the canonical on-machine git repo of custom bundles.
// Constant (not configurable): the flake override
// --override-input bundles git+file://<RepoPath> must match the repo the
// daemon reads, so both live here and in the rebuild command.
const RepoPath = "/var/lib/omahab/bundles"

// Reserved custom port range: custom bundles must listen inside
// 20000-20999 so they can never collide with curated native ports
// (internal/apps/native_ports.go) or well-known service ports.
const (
	CustomPortMin = 20000
	CustomPortMax = 20999
)

// Custom ports are unique across the merged catalog, enforced in Validate.
const (
	bundleJSONName = "bundle.json"
	moduleNixName  = "module.nix"
	disabledName   = "disabled"
)

var idDirRe = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)

// Source labels the origin of a bundle.
type Source string

const (
	SourceCatalog Source = "catalog"
	SourceCustom  Source = "custom"
)

// Bundle pairs a validated catalog bundle with its origin.
type Bundle struct {
	apps.Bundle
	Source Source
}

// Merged is the validated merged catalog: curated entries first (in
// assembled-catalog order), then custom entries (sorted by id).
type Merged struct {
	byID  map[string]Bundle
	order []string
}

// Bundles lists every merged bundle in order.
func (m *Merged) Bundles() []Bundle {
	out := make([]Bundle, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, m.byID[id])
	}
	return out
}

// Get returns the merged bundle with the given id.
func (m *Merged) Get(id string) (Bundle, bool) {
	b, ok := m.byID[id]
	return b, ok
}

// IDs lists merged bundle ids in order.
func (m *Merged) IDs() []string {
	out := make([]string, len(m.order))
	copy(out, m.order)
	return out
}

// Merge validates and merges a custom bundle JSON document (one bundle
// object per file, loader-assigned source) into a curated catalog. It
// enforces the custom contract: no id or unit collisions with curated
// entries, ports inside the reserved custom range, and cross-custom
// uniqueness of ids, units, and ports.
func Merge(curated *apps.Catalog, customDocs []CustomDoc) (*Merged, error) {
	m := &Merged{byID: make(map[string]Bundle)}
	if curated == nil {
		curated = &apps.Catalog{}
	}

	var problems []string
	seenUnit := map[string]string{} // unit -> bundle id that owns it
	seenPort := map[int]string{}    // port -> bundle id that owns it

	for _, b := range curated.Bundles() {
		nb, err := b.Validate()
		if err != nil {
			return nil, fmt.Errorf("curated bundle %q: %w", b.ID, err)
		}
		m.byID[nb.ID] = Bundle{Bundle: nb, Source: SourceCatalog}
		m.order = append(m.order, nb.ID)
		for _, u := range nb.Units {
			seenUnit[u] = nb.ID
		}
		if nb.Port > 0 {
			seenPort[nb.Port] = nb.ID
		}
	}

	for _, doc := range customDocs {
		cb, err := ValidateCustom(doc.Bundle, doc.Dir)
		if err != nil {
			var ve *apps.ValidationError
			if errors.As(err, &ve) {
				problems = append(problems, fmt.Sprintf("bundle %q: %s", doc.Bundle.ID, ve.Error()))
				continue
			}
			return nil, err
		}
		if _, dup := m.byID[cb.ID]; dup {
			problems = append(problems, fmt.Sprintf("bundle %q: id collides with an existing bundle", cb.ID))
			continue
		}
		for _, u := range cb.Units {
			if owner, clash := seenUnit[u]; clash {
				problems = append(problems, fmt.Sprintf("bundle %q: unit %q collides with bundle %q", cb.ID, u, owner))
			} else {
				seenUnit[u] = cb.ID
			}
		}
		if cb.Port > 0 {
			if owner, clash := seenPort[cb.Port]; clash {
				problems = append(problems, fmt.Sprintf("bundle %q: port %d collides with bundle %q", cb.ID, cb.Port, owner))
			} else {
				seenPort[cb.Port] = cb.ID
			}
		}
		m.byID[cb.ID] = Bundle{Bundle: cb, Source: SourceCustom}
		m.order = append(m.order, cb.ID)
	}
	if len(problems) > 0 {
		return nil, &apps.ValidationError{Problems: problems}
	}
	return m, nil
}

// CustomDoc is one custom bundle document read from the repo at HEAD.
type CustomDoc struct {
	Dir string
	apps.Bundle
}

// ValidateCustom enforces the custom-bundle contract on top of the shared
// catalog validation: required fields (name, units, port, health_check),
// the reserved custom port range, and a module.nix sibling in the repo at
// HEAD. Secrets never belong in module.nix (env flows through
// /var/lib/omahab/appenv/<id>.env with ConditionPathExists, exactly like
// curated gated bundles); that rule is documented, not machine-checked —
// module.nix is arbitrary Nix evaluated in the system closure.
func ValidateCustom(b apps.Bundle, dir string) (apps.Bundle, error) {
	var problems []string
	if dir != "" && !idDirRe.MatchString(dir) {
		problems = append(problems, fmt.Sprintf("bundle directory %q must be 2-63 chars of [a-z0-9-], starting with a letter and ending alphanumeric", dir))
	}
	if b.ID == "" {
		problems = append(problems, "id is required")
	}
	if strings.TrimSpace(b.Name) == "" {
		problems = append(problems, "name is required")
	}
	if len(b.Units) == 0 {
		problems = append(problems, "units is required")
	}
	if b.Port == 0 {
		problems = append(problems, "port is required for custom bundles (reserved range 20000-20999)")
	} else if b.Port < CustomPortMin || b.Port > CustomPortMax {
		problems = append(problems, fmt.Sprintf("port %d outside the reserved custom range %d-%d", b.Port, CustomPortMin, CustomPortMax))
	}
	if b.HealthCheck.Kind == "" {
		problems = append(problems, "health_check.kind is required for custom bundles")
	}
	if len(problems) > 0 {
		return apps.Bundle{}, &apps.ValidationError{Problems: problems}
	}
	nb, err := b.Validate()
	if err != nil {
		return apps.Bundle{}, err
	}
	return nb, nil
}

// ReadCustomBundleFiles reads one custom bundle's committed files at HEAD:
// bundle.json content and whether module.nix and the disabled marker exist.
// It never reads the dirty worktree. The returned bundle is unvalidated;
// callers pass it through ValidateCustom/Merge.
func ReadCustomBundleFiles(ctx context.Context, repo, id string) (bundleJSON []byte, hasModuleNix, disabled bool, err error) {
	if !idDirRe.MatchString(id) {
		return nil, false, false, &apps.ValidationError{Problems: []string{fmt.Sprintf("bundle id %q is not a valid directory name", id)}}
	}
	bundleJSON, err = gitShow(ctx, repo, fmt.Sprintf("%s/%s", id, bundleJSONName))
	if err != nil {
		return nil, false, false, err
	}
	hasModuleNix, err = gitPathExists(ctx, repo, fmt.Sprintf("%s/%s", id, moduleNixName))
	if err != nil {
		return nil, false, false, err
	}
	disabled, err = gitPathExists(ctx, repo, fmt.Sprintf("%s/%s", id, disabledName))
	if err != nil {
		return nil, false, false, err
	}
	if !hasModuleNix {
		return bundleJSON, hasModuleNix, disabled, &apps.ValidationError{Problems: []string{
			fmt.Sprintf("bundle %q is missing module.nix in %s", id, repo),
		}}
	}
	return bundleJSON, hasModuleNix, disabled, nil
}

// ListCustomIDs lists committed bundle directories in the repo at HEAD.
func ListCustomIDs(ctx context.Context, repo string) ([]string, error) {
	out, err := gitRun(ctx, repo, "ls-tree", "--name-only", "-d", "HEAD")
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		ids = append(ids, line)
	}
	sort.Strings(ids)
	return ids, nil
}

// LoadMerged loads the curated catalog from the assembled catalog.json and
// merges committed custom bundles from the repo at HEAD. A missing curated
// file yields an empty curated set (mirrors backend fail-closed semantics
// at a higher level); custom bundle problems fail the load loudly.
// Disabled custom bundles (bundles/<id>/disabled) are excluded.
func LoadMerged(ctx context.Context, catalogPath, repoPath string) (*Merged, error) {
	var curated *apps.Catalog
	if catalogPath != "" {
		c, err := apps.LoadCatalogFile(catalogPath)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("curated catalog %s: %w", catalogPath, err)
			}
			curated = &apps.Catalog{}
		} else {
			curated = c
		}
	} else {
		curated = &apps.Catalog{}
	}

	docs, err := loadCustomDocs(ctx, repoPath)
	if err != nil {
		return nil, err
	}
	return Merge(curated, docs)
}

func loadCustomDocs(ctx context.Context, repoPath string) ([]CustomDoc, error) {
	if repoPath == "" {
		return nil, nil
	}
	if _, err := os.Stat(repoPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No custom bundles yet: not an error.
			return nil, nil
		}
		return nil, err
	}
	ids, err := ListCustomIDs(ctx, repoPath)
	if err != nil {
		return nil, err
	}
	docs := make([]CustomDoc, 0, len(ids))
	for _, id := range ids {
		raw, hasModule, disabled, err := ReadCustomBundleFiles(ctx, repoPath, id)
		if err != nil {
			return nil, fmt.Errorf("custom bundle %q: %w", id, err)
		}
		if disabled {
			continue
		}
		if !hasModule {
			return nil, fmt.Errorf("custom bundle %q: missing module.nix", id)
		}
		b, err := apps.ParseBundleBytes(raw)
		if err != nil {
			return nil, fmt.Errorf("custom bundle %q: %w", id, err)
		}
		docs = append(docs, CustomDoc{Dir: id, Bundle: b})
	}
	return docs, nil
}

func gitShow(ctx context.Context, repo, path string) ([]byte, error) {
	out, err := gitExecOutput(ctx, repo, []string{"show", "HEAD:" + path})
	if err != nil {
		return nil, fmt.Errorf("git show HEAD:%s in %s: %w", path, repo, err)
	}
	return out, nil
}

func gitPathExists(ctx context.Context, repo, path string) (bool, error) {
	err := gitExecRun(ctx, repo, "cat-file", "-e", "HEAD:"+path)
	return err == nil, nil
}

func gitRun(ctx context.Context, repo string, args ...string) (string, error) {
	out, err := gitExecOutput(ctx, repo, args)
	if err != nil {
		return "", fmt.Errorf("git %s in %s: %w", strings.Join(args, " "), repo, err)
	}
	return string(out), nil
}

// BundleJSONPath is the per-bundle metadata file inside a bundle dir.
func BundleJSONPath(dir string) string { return filepath.Join(dir, bundleJSONName) }

// ModuleNixPath is the per-bundle NixOS module inside a bundle dir.
func ModuleNixPath(dir string) string { return filepath.Join(dir, moduleNixName) }

// DisabledPath is the per-bundle disable marker inside a bundle dir.
func DisabledPath(dir string) string { return filepath.Join(dir, disabledName) }
