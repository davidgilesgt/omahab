package controlplane

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/bundles"
)

// scopeRecords turns the resolved scope set into v2 manifest scope
// records: paths, staged artifacts with sha256 hashes, bundle identity
// for app scopes, and the server-config requirement on every app scope.
func (p *capturePlan) scopeRecords(ctx context.Context, scopes []string, bundleByID map[string]apps.Bundle, staging, customRepo string) []backups.ManifestScope {
	var out []backups.ManifestScope
	for _, name := range scopes {
		rec := backups.ManifestScope{Name: name}
		switch {
		case name == backups.ScopeServerConfig:
			rec.Paths = p.scopePaths(name)
			rec.Artifacts = stagedArtifacts(staging, controlCheckpointFiles(staging))
			if v, err := runningPostgresMajor(); err == nil {
				rec.PostgresMajor = v
			}
		case isDataScope(name):
			rec.Paths = p.scopePaths(name)
		default:
			if id, ok := backups.ScopeBundleID(name); ok {
				bd, found := bundleByID[id]
				if !found {
					continue
				}
				rec.Requires = []string{backups.ScopeServerConfig}
				rec.Paths = p.scopePaths(name)
				var vols []backups.ManifestDisk
				for _, v := range bd.Data {
					vols = append(vols, backups.ManifestDisk{Volume: bd.ID + ":" + v.Name, Path: v.Path})
				}
				rec.Volumes = vols
				var dbs []backups.ManifestDatabase
				for _, dbName := range bd.Backup.Databases {
					file := dbName + ".pgdump"
					sum, err := backups.SHA256File(filepath.Join(staging, file))
					if err != nil {
						continue
					}
					dbs = append(dbs, backups.ManifestDatabase{Name: dbName, File: file, SHA256: sum})
					if v, err := runningPostgresMajor(); err == nil {
						rec.PostgresMajor = v
					}
				}
				rec.Databases = dbs
				if len(dbs) == 0 && len(rec.Artifacts) == 0 {
					// File-only app scope: the staged tree carries the
					// definition when custom.
					rec.Artifacts = nil
				}
				rec.SecretNames = bd.SecretSources
				rec.Bundle = bundleRecord(bd, staging, customRepo)
			}
		}
		out = append(out, rec)
	}
	return out
}

// scopePaths lists the live source paths captured for one scope.
func (p *capturePlan) scopePaths(scope string) []string {
	switch scope {
	case backups.ScopeServerSync:
		return []string{"sync"}
	case backups.ScopeServerProjects:
		return []string{"projects"}
	case backups.ScopeMachineBackups:
		return []string{"machine-backups"}
	}
	if id, ok := backups.ScopeBundleID(scope); ok {
		for _, bd := range p.bundles {
			if bd.ID == id {
				var out []string
				for _, v := range bd.Data {
					out = append(out, v.Path)
				}
				return out
			}
		}
	}
	return nil
}

// controlCheckpointFiles lists the staged server-config files in a
// stable order.
func controlCheckpointFiles(staging string) []string {
	var names []string
	entries, err := os.ReadDir(staging)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch e.Name() {
		case "control.db", "recovery.kit", "storage.json":
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// stagedArtifacts hashes staged files into artifact records.
func stagedArtifacts(staging string, files []string) []backups.ManifestArtifact {
	var out []backups.ManifestArtifact
	for _, name := range files {
		sum, err := backups.SHA256File(filepath.Join(staging, name))
		if err != nil {
			continue
		}
		kind := backups.ArtifactFile
		switch {
		case name == "control.db":
			kind = backups.ArtifactSQLiteDump
		case strings.HasSuffix(name, ".pgdump"):
			kind = backups.ArtifactPGDump
		}
		out = append(out, backups.ManifestArtifact{File: name, SHA256: sum, Kind: kind})
	}
	return out
}

// runningPostgresMajor reports the major version of the running
// PostgreSQL cluster from the environment the module exports.
func runningPostgresMajor() (int, error) {
	v := strings.TrimSpace(os.Getenv("OMAHAB_PG_MAJOR"))
	if v == "" {
		return 0, fmt.Errorf("OMAHAB_PG_MAJOR not set")
	}
	return strconv.Atoi(v)
}

// isDataScope reports whether name is a server-owned data scope.
func isDataScope(name string) bool {
	return name == backups.ScopeServerSync || name == backups.ScopeServerProjects || name == backups.ScopeMachineBackups
}

// bundleRecord records where an app scope's definition lives. Custom
// bundles carry their definition files inside the snapshot at
// bundles/<id>/; catalog bundles need none. The repo argument is the
// custom-bundles repo backing the capture (empty when none exists).
func bundleRecord(bd apps.Bundle, staging, repo string) *backups.ManifestBundle {
	def := filepath.Join(staging, "bundles", bd.ID)
	if _, err := os.Stat(def); err != nil {
		return &backups.ManifestBundle{ID: bd.ID, Source: string(bundles.SourceCatalog)}
	}
	rev := ""
	if repo != "" {
		if out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output(); err == nil {
			rev = strings.TrimSpace(string(out))
		}
	}
	return &backups.ManifestBundle{
		ID:             bd.ID,
		Source:         string(bundles.SourceCustom),
		DefinitionPath: "bundles/" + bd.ID,
		DefinitionRev:  rev,
	}
}

// stageCustomBundleDefinitions copies the committed files of each custom
// app bundle in the plan into staging bundles/<id>/ via git archive at
// the repo HEAD. Catalog bundles contribute nothing.
func (b *Backend) stageCustomBundleDefinitions(ctx context.Context, plan *capturePlan, staging string) error {
	repo := b.customBundlesRepo()
	if repo == "" {
		return nil
	}
	custom := map[string]bool{}
	for _, id := range b.customBundleIDs(ctx) {
		custom[id] = true
	}
	for _, bd := range plan.bundles {
		if !custom[bd.ID] {
			continue
		}
		dest := filepath.Join(staging, "bundles", bd.ID)
		if err := os.MkdirAll(dest, 0o700); err != nil {
			return err
		}
		// git archive HEAD:<id>/ | tar -x -C <dest>: HEAD:<id>/ holds
		// the bundle's committed files without the leading directory,
		// so extraction lands the definition in staging/bundles/<id>.
		archive := exec.CommandContext(ctx, "git", "-C", repo, "archive", "--format=tar", "HEAD:"+bd.ID+"/")
		extract := exec.CommandContext(ctx, "tar", "-x", "-C", dest)
		pipe, err := extract.StdinPipe()
		if err != nil {
			return err
		}
		archive.Stdout = pipe
		var errOut strings.Builder
		archive.Stderr = &errOut
		extract.Stderr = &errOut
		if err := archive.Start(); err != nil {
			_ = pipe.Close()
			return fmt.Errorf("git archive %s: %w", bd.ID, err)
		}
		if err := extract.Start(); err != nil {
			_ = pipe.Close()
			_ = archive.Wait()
			return fmt.Errorf("extract custom bundle %s: %w", bd.ID, err)
		}
		_ = pipe.Close()
		aerr := archive.Wait()
		xerr := extract.Wait()
		if aerr != nil || xerr != nil {
			return fmt.Errorf("stage custom bundle %s: %v: %s", bd.ID, joinErrors(aerr, xerr), errOut.String())
		}
	}
	return nil
}

func joinErrors(errs ...error) error {
	var live []error
	for _, e := range errs {
		if e != nil {
			live = append(live, e)
		}
	}
	if len(live) == 0 {
		return nil
	}
	if len(live) == 1 {
		return live[0]
	}
	var msg strings.Builder
	for i, e := range live {
		if i > 0 {
			msg.WriteString("; ")
		}
		msg.WriteString(e.Error())
	}
	return fmt.Errorf("%s", msg.String())
}

// customBundlesRepo reports the custom-bundles repo directory backing
// capture staging: the configured custom bundles dir when present, else
// the fixed on-disk location when it exists.
func (b *Backend) customBundlesRepo() string {
	if dir := strings.TrimSpace(b.cfg.CustomBundlesDir); dir != "" {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
		return ""
	}
	if bundles.RepoExists() {
		return bundles.RepoPath
	}
	return ""
}

// customBundleIDs lists the bundle ids the merged catalog assigns to the
// custom repo (disabled bundles are excluded by LoadMerged), so captures
// archive exactly the definitions the runtime serves.
func (b *Backend) customBundleIDs(ctx context.Context) []string {
	m, err := bundles.LoadMerged(ctx, b.cfg.CatalogPath, b.customBundlesRepo())
	if err != nil {
		return nil
	}
	var ids []string
	for _, bdl := range m.Bundles() {
		if bdl.Source == bundles.SourceCustom {
			ids = append(ids, bdl.Bundle.ID)
		}
	}
	return ids
}

// bundleSourceByID reports the loader-assigned source of a bundle id by
// consulting the merged catalog; ids absent from the merged view report
// catalog (they are curated or unknown to the custom repo).
func (b *Backend) bundleSourceByID(ctx context.Context, id string) (string, bool) {
	m, err := bundles.LoadMerged(ctx, b.cfg.CatalogPath, b.customBundlesRepo())
	if err != nil {
		return "", false
	}
	if bdl, ok := m.Get(id); ok {
		return string(bdl.Source), true
	}
	return string(bundles.SourceCatalog), false
}
