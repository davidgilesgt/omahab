package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/bundles"
	"github.com/omahab/omahab/internal/events"
)

// Custom bundles (DESIGN §6.1): the daemon serves the merged catalog
// (curated + committed custom bundles) and drives the same
// internal/bundles.Service the `omahab bundle` CLI uses — one service path
// for panel and CLI. Apply itself runs in the omahab-bundle-apply oneshot
// unit (nix/module.nix) because the switch restarts omahabd; this daemon
// only stages the operation, starts the unit, and reports journal status.

// bundlesEventSink adapts the control-plane event service onto the
// bundles.EventSink interface.
type bundlesEventSink struct {
	svc *events.Service
}

func (s *bundlesEventSink) Publish(ctx context.Context, in bundles.PublishInput) {
	if s.svc == nil {
		return
	}
	_, _ = s.svc.Publish(ctx, events.PublishInput{
		Type:       in.Type,
		Severity:   in.Severity,
		Message:    in.Message,
		ResourceID: in.BundleID,
	})
}

// loadMergedCatalog loads the merged catalog for serving. Custom bundle
// problems fail loudly (a broken custom bundle must not silently vanish
// from the panel); a missing curated file yields an empty curated set
// (mirrors the legacy fail-closed behavior).
func (b *Backend) loadMergedCatalog(ctx context.Context) (*apps.Catalog, error) {
	if b.cfg.CustomBundlesDir == "" {
		return loadCuratedCatalog(b.cfg.CatalogPath)
	}
	m, err := bundles.LoadMerged(ctx, b.cfg.CatalogPath, b.cfg.CustomBundlesDir)
	if err != nil {
		return nil, err
	}
	return mergedToCatalog(m), nil
}

// loadCuratedCatalog keeps the legacy fail-closed behavior for the
// curated-only view: missing file → empty catalog.
func loadCuratedCatalog(path string) (*apps.Catalog, error) {
	if path == "" {
		return apps.NewCatalog()
	}
	c, err := apps.LoadCatalogFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return apps.NewCatalog()
		}
		return nil, err
	}
	return c, nil
}

// mergedToCatalog projects the merged view onto the apps service catalog.
// LoadMerged already validated every entry; NewCatalog re-runs the same
// validation on the same data, so its error is unreachable and the empty
// catalog is a safe last resort.
func mergedToCatalog(m *bundles.Merged) *apps.Catalog {
	list := m.Bundles()
	out := make([]apps.Bundle, 0, len(list))
	for _, b := range list {
		out = append(out, b.Bundle)
	}
	c, err := apps.NewCatalog(out...)
	if err != nil {
		// Unreachable: LoadMerged validated every entry with the same
		// contract. Serve the empty catalog rather than crash the daemon.
		empty, _ := apps.NewCatalog()
		return empty
	}
	return c
}

// reloadMergedCatalog re-reads the merged catalog into the apps service
// after a successful apply (new/removed/disabled custom bundles become
// visible without restarting omahabd).
func (b *Backend) reloadMergedCatalog(ctx context.Context) error {
	if b.apps == nil {
		return nil
	}
	c, err := b.loadMergedCatalog(ctx)
	if err != nil {
		return err
	}
	b.apps.SetCatalog(c)
	return nil
}

// bundlesSvc returns the custom-bundles repo service, or nil when custom
// bundles are not configured.
func (b *Backend) bundlesSvc() *bundles.Service {
	if b.bundles == nil && b.cfg.CustomBundlesDir != "" {
		svc, err := bundles.NewService(b.cfg.CustomBundlesDir, nil, &bundlesEventSink{b.events})
		if err == nil {
			b.bundles = svc
		}
	}
	return b.bundles
}

// --- admin API operations ---

// ListBundles returns the merged bundle list enriched with repo state:
// source, disabled marker, dirty (uncommitted changes), and the last
// apply outcome.
func (b *Backend) ListBundles(ctx context.Context) ([]apitypes.BundleAdmin, error) {
	var customState map[string]bundles.StatusItem
	repoDirty := false
	if svc := b.bundlesSvc(); svc != nil {
		st, err := svc.Snapshot(ctx)
		if err != nil {
			return nil, translateError(err)
		}
		repoDirty = st.Dirty
		customState = make(map[string]bundles.StatusItem, len(st.Bundles))
		for _, item := range st.Bundles {
			customState[item.ID] = item
		}
	}
	merged, err := bundles.LoadMerged(ctx, b.cfg.CatalogPath, b.cfg.CustomBundlesDir)
	if err != nil {
		return nil, translateError(err)
	}
	lastApply := b.lastBundleApplyStatus()
	out := make([]apitypes.BundleAdmin, 0, len(merged.Bundles()))
	for _, bdl := range merged.Bundles() {
		item := apitypes.BundleAdmin{
			ID:        bdl.ID,
			Name:      bdl.Name,
			Source:    string(bdl.Source),
			Port:      bdl.Port,
			Units:     bdl.Units,
			LastApply: lastApply,
		}
		if bdl.Source == bundles.SourceCustom {
			if st, ok := customState[bdl.ID]; ok {
				item.Disabled = st.Disabled
				item.Dirty = repoDirty
			}
		}
		out = append(out, item)
	}
	// Disabled custom bundles are excluded from the merged catalog; append
	// them so the panel can list and re-enable them.
	if customState != nil {
		mergedIDs := make(map[string]bool, len(out))
		for _, item := range out {
			mergedIDs[item.ID] = true
		}
		var disabledRows []apitypes.BundleAdmin
		for id, st := range customState {
			if !st.Disabled || mergedIDs[id] {
				continue
			}
			row := apitypes.BundleAdmin{ID: id, Source: string(bundles.SourceCustom), Disabled: true, Dirty: repoDirty, LastApply: lastApply}
			if svc := b.bundlesSvc(); svc != nil {
				if bundleJSON, _, _, err := bundles.ReadCustomBundleFiles(ctx, svc.Repo(), id); err == nil {
					if p, perr := apps.ParseBundleBytes(bundleJSON); perr == nil {
						row.Name, row.Port, row.Units = p.Name, p.Port, p.Units
					}
				}
			}
			disabledRows = append(disabledRows, row)
		}
		sort.Slice(disabledRows, func(i, j int) bool { return disabledRows[i].ID < disabledRows[j].ID })
		out = append(out, disabledRows...)
	}
	return out, nil
}

// AddCustomBundle validates and writes a new custom bundle into the repo
// and commits it. The bundle is not active until an apply.
func (b *Backend) AddCustomBundle(ctx context.Context, req apitypes.CreateBundleRequest) (apitypes.BundleAdmin, error) {
	svc := b.bundlesSvc()
	if svc == nil {
		return apitypes.BundleAdmin{}, translateError(fmt.Errorf("%w: custom bundles are not configured", ErrNotConfigured))
	}
	// Validate the merged result before writing: the new bundle must not
	// collide with curated entries or existing custom bundles.
	bundleJSON := []byte(req.BundleJSON)
	parsed, err := apps.ParseBundleBytes(bundleJSON)
	if err != nil {
		return apitypes.BundleAdmin{}, translateError(err)
	}
	if req.ID != "" && strings.TrimSpace(req.ID) != parsed.ID {
		return apitypes.BundleAdmin{}, translateError(fmt.Errorf("%w: request id %q does not match bundle.json id %q", apitypes.ErrValidation, req.ID, parsed.ID))
	}
	if err := svc.EnsureRepo(ctx); err != nil {
		return apitypes.BundleAdmin{}, translateError(err)
	}
	committed, err := committedCustomDocs(ctx, svc.Repo())
	if err != nil {
		return apitypes.BundleAdmin{}, translateError(err)
	}
	if _, err := bundles.Merge(curatedCatalogForMerge(b.cfg.CatalogPath), append(committed, bundles.CustomDoc{Dir: parsed.ID, Bundle: parsed})); err != nil {
		return apitypes.BundleAdmin{}, translateError(err)
	}
	id, err := svc.AddFiles(ctx, bundleJSON, []byte(req.ModuleNix))
	if err != nil {
		return apitypes.BundleAdmin{}, translateError(err)
	}
	if commit, err := svc.Commit(ctx, "omahab api bundle add "+id); err != nil && commit == "" {
		return apitypes.BundleAdmin{}, translateError(fmt.Errorf("bundle %q added but commit failed: %w", id, err))
	}
	return apitypes.BundleAdmin{ID: id, Source: string(bundles.SourceCustom), Disabled: false, Dirty: false}, nil
}

// RemoveCustomBundle deletes bundles/<id>/ from the repo and commits the
// removal. Activation happens on the next apply.
func (b *Backend) RemoveCustomBundle(ctx context.Context, id string) error {
	svc := b.bundlesSvc()
	if svc == nil {
		return translateError(fmt.Errorf("%w: custom bundles are not configured", ErrNotConfigured))
	}
	if err := svc.Remove(ctx, id); err != nil {
		return translateError(err)
	}
	if commit, err := svc.Commit(ctx, "omahab api bundle remove "+id); err != nil && commit == "" {
		return translateError(fmt.Errorf("bundle %q removed but commit failed: %w", id, err))
	}
	return nil
}

// SetCustomBundleDisabled adds or removes the disabled marker and commits.
// Disabling takes effect at the next rebuild (the marker is committed, so
// rebuild-time exclusion and the merged catalog agree).
func (b *Backend) SetCustomBundleDisabled(ctx context.Context, id string, disabled bool) error {
	svc := b.bundlesSvc()
	if svc == nil {
		return translateError(fmt.Errorf("%w: custom bundles are not configured", ErrNotConfigured))
	}
	if err := svc.SetDisabled(ctx, id, disabled); err != nil {
		return translateError(err)
	}
	action := "enable"
	if disabled {
		action = "disable"
	}
	if commit, err := svc.Commit(ctx, "omahab api bundle "+action+" "+id); err != nil && commit == "" {
		return translateError(fmt.Errorf("bundle %q %sd but commit failed: %w", id, action, err))
	}
	return nil
}

// --- apply operations ---

const bundleApplyJournalKind = "bundle-apply"

// TriggerBundleApply stages an apply operation and starts the
// omahab-bundle-apply oneshot unit, which runs `omahab bundle apply
// --apply-staged` as root. The switch inside restarts omahabd, so the
// daemon must not run Apply itself (it would health-gate its own
// restart): the operation id travels through a tmpfs handoff file (same
// pattern as omahab-restore) and the journal under the state dir records
// progress for polling after the restart.
func (b *Backend) TriggerBundleApply(ctx context.Context) (Operation, error) {
	svc := b.bundlesSvc()
	if svc == nil {
		return Operation{}, translateError(fmt.Errorf("%w: custom bundles are not configured", ErrNotConfigured))
	}
	if err := svc.EnsureRepo(ctx); err != nil {
		return Operation{}, translateError(err)
	}
	op := newOperation(bundleApplyJournalKind, nil)
	op.Stage = "unit-started"
	op.Status = OpRunning
	if err := writeOperationJournal(b.cfg.StateDir, op); err != nil {
		return Operation{}, translateError(err)
	}
	if err := writeBundleApplyHandoff(op.ID); err != nil {
		return Operation{}, translateError(err)
	}
	out, err := execCommand("systemctl", "start", "omahab-bundle-apply.service")
	if err != nil {
		_ = os.Remove(bundleApplyHandoffPath())
		detail := fmt.Sprintf("start omahab-bundle-apply.service: %v: %s", err, strings.TrimSpace(out))
		op.Status = OpFailed
		op.Error = detail
		_ = writeOperationJournal(b.cfg.StateDir, op)
		return op, translateError(fmt.Errorf("%s", detail))
	}
	_ = writeOperationJournal(b.cfg.StateDir, op)
	return op, nil
}

// GetBundleApplyStatus polls one bundle apply operation by id. When the
// daemon restarted mid-apply (the switch does exactly that), the journal
// still says running until the CLI run records its outcome next to it.
func (b *Backend) GetBundleApplyStatus(ctx context.Context, id string) (Operation, error) {
	raw, err := os.ReadFile(operationJournalPath(b.cfg.StateDir, bundleApplyJournalKind, strings.TrimSpace(id)))
	if err != nil {
		return Operation{}, fmt.Errorf("%w: bundle apply %q not found", apitypes.ErrNotFound, id)
	}
	var op Operation
	if err := json.Unmarshal(raw, &op); err != nil {
		return Operation{}, fmt.Errorf("parse bundle apply journal: %w", err)
	}
	if op.Status == OpRunning {
		if done, status, detail := readBundleApplyOutcome(b.cfg.StateDir, op.ID); done {
			op.Status = status
			op.Error = detail
			_ = writeOperationJournal(b.cfg.StateDir, op)
		}
	}
	return op, nil
}

// lastBundleApplyStatus reports the most recent apply outcome ("" when no
// apply has run) for the bundle list view.
func (b *Backend) lastBundleApplyStatus() string {
	dir := operationsDir(b.cfg.StateDir, bundleApplyJournalKind)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var newest string
	var newestAt int64 = -1
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !e.IsDir() {
			continue
		}
		if info.ModTime().Unix() > newestAt {
			newestAt = info.ModTime().Unix()
			newest = e.Name()
		}
	}
	if newest == "" {
		return ""
	}
	op, err := b.GetBundleApplyStatus(context.Background(), newest)
	if err != nil {
		return ""
	}
	if op.Status == OpCompleted {
		return "completed"
	}
	return op.Status
}

// writeBundleApplyHandoff drops the operation id on tmpfs for the oneshot
// unit's CLI run. /run is the daemon's declared ReadWritePaths root and is
// wiped at boot; the file holds no secret material.
func writeBundleApplyHandoff(opID string) error {
	path := bundleApplyHandoffPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create runtime dir: %w", err)
	}
	body, err := json.Marshal(map[string]string{"operation_id": opID})
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

// bundleApplyHandoffPath is the tmpfs handoff consumed by
// `omahab bundle apply --apply-staged`.
func bundleApplyHandoffPath() string {
	if v := strings.TrimSpace(os.Getenv("OMAHAB_RUNTIME_DIR")); v != "" {
		return filepath.Join(v, "bundle-apply")
	}
	return "/run/omahab/bundle-apply"
}

// readBundleApplyOutcome reads the completion marker the CLI run writes
// next to the journal. done is false while the apply is still running.
func readBundleApplyOutcome(stateDir, opID string) (done bool, status string, detail string) {
	raw, err := os.ReadFile(bundleApplyOutcomePath(stateDir, opID))
	if err != nil {
		return false, "", ""
	}
	var m struct {
		Status string `json:"status"`
		Detail string `json:"detail,omitempty"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.Status == "" {
		return false, "", ""
	}
	return true, m.Status, m.Detail
}

// bundleApplyOutcomePath is where the CLI apply records its result.
func bundleApplyOutcomePath(stateDir, opID string) string {
	return filepath.Join(operationsDir(stateDir, bundleApplyJournalKind), opID, "outcome.json")
}

// committedCustomDocs lists the committed custom bundle documents (the
// HEAD view) for merge validation.
func committedCustomDocs(ctx context.Context, repo string) ([]bundles.CustomDoc, error) {
	ids, err := bundles.ListCustomIDs(ctx, repo)
	if err != nil {
		return nil, err
	}
	docs := make([]bundles.CustomDoc, 0, len(ids))
	for _, id := range ids {
		raw, hasModule, disabled, err := bundles.ReadCustomBundleFiles(ctx, repo, id)
		if err != nil || disabled || !hasModule {
			continue
		}
		b, err := apps.ParseBundleBytes(raw)
		if err != nil {
			return nil, fmt.Errorf("custom bundle %q: %w", id, err)
		}
		docs = append(docs, bundles.CustomDoc{Dir: id, Bundle: b})
	}
	return docs, nil
}

// curatedCatalogForMerge loads the curated catalog for merge validation; a
// missing file means an empty curated set (fail closed, no bundles).
func curatedCatalogForMerge(path string) *apps.Catalog {
	c, err := loadCuratedCatalog(path)
	if err != nil {
		return &apps.Catalog{}
	}
	return c
}
