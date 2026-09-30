package backups

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Instance discovery for first-restore flows: find every earlier Omahab
// instance under a backup account, without any control database or local
// repository configuration. Discovery trusts exactly one credential —
// the recovery phrase (or an explicit repository password for generic
// locations) — and never persists it.

// InstanceListEntry is one discovered instance directory: its subdirectory
// name, its exact resolvable repository location, and whatever metadata
// could be read from its newest snapshot.
type InstanceListEntry struct {
	// InstanceID is the instance directory name (uXXXXXX-...).
	InstanceID string
	// Location is the exact restic repository URL for this instance.
	Location string
	// LatestSnapshotTime is the RFC3339 time of the newest readable
	// snapshot; empty when locked.
	LatestSnapshotTime string
	// Domain is the instance domain from the newest snapshot's v2
	// manifest; empty when unreadable or locked.
	Domain string
	// Locked is true when the supplied password cannot open this
	// instance's repository (skip it or use its own phrase).
	Locked bool
}

// InstanceDirLister enumerates the instance repositories below a root
// location. The Hetzner implementation lists subdirectories of the
// account's ./omahab/ over SFTP; other location schemes have no
// enumerable root.
type InstanceDirLister interface {
	// ListInstanceDirs returns the instance repository directory names
	// below repoRootLocation.
	ListInstanceDirs(ctx context.Context, repoRootLocation string) ([]string, error)
}

// manifestDumpPath is the fixed in-snapshot path of the v2 manifest:
// snapshots capture the staging tree at its absolute path, so the
// manifest always sits at backup-staging/manifest.json below the
// repository root.
const manifestDumpPath = "/" + backupStagingDir + "/" + manifestFileName

// instanceDirRe accepts exactly one safe path component: an alphanumeric
// first character, then alphanumerics, dots, underscores, or dashes.
// Slash/dotdot traversal can never name a different repository.
var instanceDirRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// JoinInstanceLocation appends one instance directory to a root location:
// sftp URLs join the path component (port and credentials preserved),
// local paths append below the root. The result is the exact repository
// URL callers must use.
func JoinInstanceLocation(root, instanceID string) (string, error) {
	root = strings.TrimSpace(root)
	instanceID = strings.TrimSpace(instanceID)
	if root == "" {
		return "", fmt.Errorf("%w: repository root location is required", ErrInvalid)
	}
	if !instanceDirRe.MatchString(instanceID) {
		return "", fmt.Errorf("%w: invalid instance directory %q", ErrInvalid, instanceID)
	}
	if strings.HasPrefix(strings.ToLower(root), "sftp://") {
		u, err := url.Parse(root)
		if err != nil || u.Host == "" {
			return "", fmt.Errorf("%w: invalid sftp root location %q", ErrInvalid, root)
		}
		u.Path = strings.TrimSuffix(u.Path, "/") + "/" + instanceID
		return u.String(), nil
	}
	if !strings.HasPrefix(root, "/") {
		return "", fmt.Errorf("%w: invalid repository root location %q", ErrInvalid, root)
	}
	return strings.TrimSuffix(root, "/") + "/" + instanceID, nil
}

// InstanceIDFromLocation derives a display identifier from a repository
// location: the last path segment for URLs and absolute paths.
func InstanceIDFromLocation(location string) string {
	loc := strings.TrimRight(strings.TrimSpace(location), "/")
	if loc == "" {
		return ""
	}
	if i := strings.LastIndex(loc, "/"); i >= 0 {
		return loc[i+1:]
	}
	return loc
}

// ListInstances discovers every earlier Omahab instance below repoRoot.
// Each enumerated directory is probed read-only with the supplied
// repository password: the newest snapshot's time and the v2 manifest's
// instance domain surface when the password opens the repository;
// otherwise the instance reports as locked. The password is passed to
// the runner environment only.
func ListInstances(ctx context.Context, d InstanceDirLister, runner Runner, repoRootLocation, password string) ([]InstanceListEntry, error) {
	if strings.TrimSpace(repoRootLocation) == "" {
		return nil, fmt.Errorf("%w: repository root location is required", ErrInvalid)
	}
	if strings.TrimSpace(password) == "" {
		return nil, fmt.Errorf("%w: repository password is required", ErrInvalid)
	}
	dirs, err := d.ListInstanceDirs(ctx, repoRootLocation)
	if err != nil {
		return nil, err
	}
	out := make([]InstanceListEntry, 0, len(dirs))
	seen := map[string]bool{}
	for _, id := range dirs {
		id = strings.TrimSpace(id)
		// Enumerated names must be single safe path components; a
		// hostile listing can never redirect the probe at another
		// repository.
		if !instanceDirRe.MatchString(id) || seen[id] {
			continue
		}
		seen[id] = true
		loc, err := JoinInstanceLocation(repoRootLocation, id)
		if err != nil {
			continue
		}
		out = append(out, probeInstance(ctx, runner, id, loc, password))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstanceID < out[j].InstanceID })
	return out, nil
}

// ListSingleInstance probes one repository location read-only: the newest
// snapshot's time and the v2 manifest's instance domain surface when the
// repository password opens it; otherwise the instance reports as locked.
// It backs the generic-location form of discovery, where there is no
// account directory to enumerate.
func ListSingleInstance(ctx context.Context, runner Runner, location, password string) InstanceListEntry {
	id := InstanceIDFromLocation(location)
	return probeInstance(ctx, runner, id, location, password)
}

// probeInstance lists one repository's newest snapshot and dumps the v2
// manifest from it. Failure to open the repository marks the instance
// locked; other read errors surface in LatestSnapshotTime/Domain staying
// empty without failing discovery for the sibling instances.
func probeInstance(ctx context.Context, runner Runner, instanceID, location, password string) InstanceListEntry {
	entry := InstanceListEntry{InstanceID: instanceID, Location: location}
	repo := Repository{Location: location, Attached: true}
	creds := Credentials{Password: password}
	snaps, err := runner.Snapshots(ctx, repo, creds, 1)
	if err != nil {
		// Wrong password (or a directory that is not a repository):
		// report locked rather than aborting discovery.
		entry.Locked = true
		return entry
	}
	if len(snaps) == 0 {
		return entry
	}
	latest := snaps[0]
	entry.LatestSnapshotTime = latest.Time
	if manifest, err := dumpInstanceManifest(ctx, runner, repo, creds, latest.ID); err == nil {
		entry.Domain = manifest.Instance.Domain
	}
	return entry
}

// dumpInstanceManifest reads the v2 manifest from one snapshot via
// `restic dump <snap> /var/lib/omahab/backup-staging/manifest.json`.
// The password never appears in argv; it rides the runner environment.
func dumpInstanceManifest(ctx context.Context, runner Runner, repo Repository, creds Credentials, snapshotID string) (SnapshotManifest, error) {
	var m SnapshotManifest
	dumper, ok := runner.(ManifestDumper)
	if !ok {
		return m, fmt.Errorf("%w: runner cannot dump snapshot files", ErrInvalid)
	}
	raw, err := dumper.Dump(ctx, repo, creds, snapshotID, manifestDumpPath)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, fmt.Errorf("parse manifest from snapshot %s: %w", snapshotID, err)
	}
	if err := m.Validate(); err != nil {
		return m, fmt.Errorf("manifest from snapshot %s: %w", snapshotID, err)
	}
	return m, nil
}

// ManifestDumper reads one file out of a snapshot. CommandRunner
// implements it; the recovery fakes implement it in tests.
type ManifestDumper interface {
	Dump(ctx context.Context, repo Repository, creds Credentials, snapshotID, path string) ([]byte, error)
}
