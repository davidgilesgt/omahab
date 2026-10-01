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
	"github.com/omahab/omahab/internal/domain"
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
//
// Mutations are refused while a bundle apply is staged or running: the
// apply builds from a pinned revision, so a commit landing mid-apply
// would desynchronize the merged catalog (and the fresh health probes
// read by the apply's health gate) from the generation being activated.
func (b *Backend) AddCustomBundle(ctx context.Context, req apitypes.CreateBundleRequest) (apitypes.BundleAdmin, error) {
	if err := b.refuseMutationsDuringApply(); err != nil {
		return apitypes.BundleAdmin{}, err
	}
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
// removal. Activation happens on the next apply. Refused while a bundle
// apply is in flight (see AddCustomBundle).
func (b *Backend) RemoveCustomBundle(ctx context.Context, id string) error {
	if err := b.refuseMutationsDuringApply(); err != nil {
		return err
	}
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
// rebuild-time exclusion and the merged catalog agree). Refused while a
// bundle apply is in flight (see AddCustomBundle).
//
// Re-enabling a disabled bundle validates the would-be merged result first
// (bundle.json at HEAD, full collision and custom-contract checks): an
// invalid or colliding enablement is rejected before anything is written,
// so the previously valid committed state survives untouched.
func (b *Backend) SetCustomBundleDisabled(ctx context.Context, id string, disabled bool) error {
	if err := b.refuseMutationsDuringApply(); err != nil {
		return err
	}
	svc := b.bundlesSvc()
	if svc == nil {
		return translateError(fmt.Errorf("%w: custom bundles are not configured", ErrNotConfigured))
	}
	if !disabled {
		if err := b.validateEnableCustom(ctx, svc, id); err != nil {
			return translateError(err)
		}
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

// validateEnableCustom checks that enabling the disabled custom bundle id
// yields a valid merged catalog. It reads the bundle's committed files (the
// same HEAD view an apply would build from), so a broken bundle.json, a
// missing module.nix, or an id/unit/port collision with the curated catalog
// or another custom bundle is rejected before the disabled marker is
// removed.
func (b *Backend) validateEnableCustom(ctx context.Context, svc *bundles.Service, id string) error {
	if err := svc.EnsureRepo(ctx); err != nil {
		return err
	}
	bundleJSON, hasModule, _, err := bundles.ReadCustomBundleFiles(ctx, svc.Repo(), id)
	if err != nil {
		return err
	}
	if !hasModule {
		return fmt.Errorf("%w: bundle %q is missing module.nix", apitypes.ErrValidation, id)
	}
	parsed, err := apps.ParseBundleBytes(bundleJSON)
	if err != nil {
		return fmt.Errorf("%w: bundle %q: %v", apitypes.ErrValidation, id, err)
	}
	if parsed.ID != id {
		return fmt.Errorf("%w: bundle %q holds bundle.json id %q", apitypes.ErrValidation, id, parsed.ID)
	}
	committed, err := committedCustomDocs(ctx, svc.Repo())
	if err != nil {
		return err
	}
	enabled := append([]bundles.CustomDoc{}, committed...)
	enabled = append(enabled, bundles.CustomDoc{Dir: id, Bundle: parsed})
	if _, err := bundles.Merge(curatedCatalogForMerge(b.cfg.CatalogPath), enabled); err != nil {
		return err
	}
	return nil
}

// probeUnitActiveFunc is the unit-state check behind ProbeBundleHealth
// (test seam; production uses systemctl is-active).
var probeUnitActiveFunc = func(ctx context.Context, unit string) (bool, error) {
	out, err := execCommand("systemctl", "is-active", "--quiet", unit)
	if err != nil {
		_ = out
		return false, nil
	}
	return true, nil
}

// probeHTTPHealthFunc is the loopback HTTP probe behind ProbeBundleHealth
// (test seam; production uses the shared apps probe).
var probeHTTPHealthFunc = func(ctx context.Context, hc apps.HealthCheck, port int) (domain.Health, error) {
	return apps.ProbeHealth(ctx, hc, port)
}

// ProbeBundleHealth runs one fresh health probe of the bundle's currently
// configured services. It answers POST /api/v1/bundles/{id}/health: 204
// only when the probe succeeds right now. It deliberately does NOT touch
// the applications database — no application row is required (the probe
// runs for freshly defined bundles that were never installed through the
// apps service) and no stored health value is consulted or updated.
//
// An empty revision probes the current committed service configuration
// (the plain panel use). A nonempty revision must be a full 40-hex commit
// hash of the custom-bundles repo: the probe then uses the bundle's
// definition pinned at exactly that immutable revision — the one the
// apply built — so a commit landing after the build cannot change which
// units and health check the gate observes.
//
// The probe loads the merged view fresh on every call, so it observes
// the currently configured services even right after a daemon restart or
// mid-restore. Every declared unit must be active; an HTTP health check
// (with the native-port override and unix-socket probes, exactly like
// the systemd runner's observation logic) must answer 2xx.
func (b *Backend) ProbeBundleHealth(ctx context.Context, id, revision string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: bundle id is required", apitypes.ErrValidation)
	}
	merged, err := bundles.LoadMergedAt(ctx, b.cfg.CatalogPath, b.cfg.CustomBundlesDir, strings.TrimSpace(revision))
	if err != nil {
		return translateError(err)
	}
	bundle, ok := merged.Get(id)
	if !ok {
		return fmt.Errorf("%w: bundle %q not found in the merged catalog", apitypes.ErrNotFound, id)
	}
	if len(bundle.Units) == 0 {
		return fmt.Errorf("%w: bundle %q declares no units", apitypes.ErrValidation, id)
	}
	for _, unit := range bundle.Units {
		active, err := probeUnitActiveFunc(ctx, unit)
		if err != nil {
			return err
		}
		if !active {
			return fmt.Errorf("probe bundle %q: unit %s is not active", id, unit)
		}
	}
	if hc := bundle.HealthCheck; hc.Kind == apps.CheckHTTP {
		port := hc.Port
		if p, ok := apps.NativePort(id); ok {
			port = p
		}
		health, err := probeHTTPHealthFunc(ctx, hc, port)
		if err != nil {
			return fmt.Errorf("probe bundle %q: %w", id, err)
		}
		if health != domain.HealthHealthy {
			return fmt.Errorf("probe bundle %q: health check reports %q", id, health)
		}
	}
	return nil
}

// refuseMutationsDuringApply rejects custom-bundle repo mutations while
// an apply reservation is live. Applies build from a pinned immutable
// revision of the bundles repo; a commit landing between stage and
// outcome would make the merged catalog (and the fresh health probes the
// apply's health gate reads) disagree with the generation being
// activated. Direct root CLI edits of the repo remain outside the
// daemon's control by design.
func (b *Backend) refuseMutationsDuringApply() error {
	if res := b.holdBundleApply(); res != nil {
		return translateError(fmt.Errorf("%w: a bundle apply operation (%s) is in progress", apitypes.ErrConflict, res.OperationID))
	}
	return nil
}

// --- apply operations ---

const bundleApplyJournalKind = "bundle-apply"

// bundleApplyReservation is the unique live-apply guarantee: at most one
// bundle apply may be staged, started, or running at a time. It is held in
// memory (the daemon serializes staging; the oneshot unit serializes runs)
// and keyed by operation id so outcomes can be attributed without phantom
// accepts. A daemon restart loses the memory slot, but /run is wiped at
// boot and reconcileBundleApplyReservation reconstructs the state from the
// journal + handoff at startup, so a stale running apply is still honored
// and no new stage can replace it.
type bundleApplyReservation struct {
	OperationID string
}

// reserveBundleApply claims the single apply slot for op. It fails with
// ErrConflict when any other apply reservation is live.
func (b *Backend) reserveBundleApply(opID string) error {
	b.bundleApplyMu.Lock()
	defer b.bundleApplyMu.Unlock()
	if b.bundleApplyRes != nil && b.bundleApplyRes.OperationID != "" {
		return fmt.Errorf("%w: a bundle apply operation (%s) is already in progress", apitypes.ErrConflict, b.bundleApplyRes.OperationID)
	}
	b.bundleApplyRes = &bundleApplyReservation{OperationID: opID}
	return nil
}

// releaseBundleApply frees the reservation held by opID. Unknown or foreign
// ids are ignored (an outcome for an apply that was never reserved cannot
// strand a newer reservation).
func (b *Backend) releaseBundleApply(opID string) {
	b.bundleApplyMu.Lock()
	defer b.bundleApplyMu.Unlock()
	if b.bundleApplyRes == nil || b.bundleApplyRes.OperationID != opID {
		return
	}
	b.bundleApplyRes = nil
}

// holdBundleApply reports whether any apply reservation is live.
func (b *Backend) holdBundleApply() *bundleApplyReservation {
	b.bundleApplyMu.Lock()
	defer b.bundleApplyMu.Unlock()
	return b.bundleApplyRes
}

// reconcileBundleApplyReservation runs at daemon startup: a leftover tmpfs
// handoff with a matching running journal is a mid-flight staged apply the
// restart cut short — adopt its reservation so a fresh stage cannot replace
// the operation the unit may still pick up. A handoff with no journal is a
// phantom (the CLI rejects it too) and is removed; a running journal with
// no handoff means the unit already consumed the handoff — adopt the
// reservation until the outcome converges.
func (b *Backend) reconcileBundleApplyReservation() {
	raw, err := os.ReadFile(bundleApplyHandoffPath())
	if err != nil {
		// No handoff on disk. Any running journal without a handoff was
		// already consumed by the unit; adopt it so concurrent stages
		// stay rejected while it converges.
		if res := b.runningApplyJournal(); res != nil {
			b.bundleApplyMu.Lock()
			b.bundleApplyRes = res
			b.bundleApplyMu.Unlock()
		}
		return
	}
	var h struct {
		OperationID string `json:"operation_id"`
	}
	_ = json.Unmarshal(raw, &h)
	handoffID := strings.TrimSpace(h.OperationID)
	if handoffID == "" {
		_ = os.Remove(bundleApplyHandoffPath())
		return
	}
	journal, err := readBundleApplyJournal(b.cfg.StateDir, handoffID)
	if err != nil || journal.Status != OpRunning {
		// Phantom handoff: no live operation behind it.
		_ = os.Remove(bundleApplyHandoffPath())
		return
	}
	b.bundleApplyMu.Lock()
	b.bundleApplyRes = &bundleApplyReservation{OperationID: handoffID}
	b.bundleApplyMu.Unlock()
}

// runningApplyJournal scans the apply journals for one still running.
// The newest running journal wins (the applies are serialized by the
// reservation, so at most one can be running).
func (b *Backend) runningApplyJournal() *bundleApplyReservation {
	dir := operationsDir(b.cfg.StateDir, bundleApplyJournalKind)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var newest string
	var newestAt int64 = -1
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !e.IsDir() {
			continue
		}
		j, err := readBundleApplyJournal(b.cfg.StateDir, e.Name())
		if err != nil || j.Status != OpRunning {
			continue
		}
		if info.ModTime().Unix() > newestAt {
			newestAt = info.ModTime().Unix()
			newest = e.Name()
		}
	}
	if newest == "" {
		return nil
	}
	return &bundleApplyReservation{OperationID: newest}
}

// readBundleApplyJournal loads one apply journal (lightweight, unshared
// with GetBundleApplyStatus so no outcome convergence happens here).
func readBundleApplyJournal(stateDir, opID string) (Operation, error) {
	raw, err := os.ReadFile(operationJournalPath(stateDir, bundleApplyJournalKind, opID))
	if err != nil {
		return Operation{}, err
	}
	var op Operation
	if err := json.Unmarshal(raw, &op); err != nil {
		return Operation{}, err
	}
	if op.ID == "" {
		return Operation{}, fmt.Errorf("bundle apply journal %q has no id", opID)
	}
	return op, nil
}

// TriggerBundleApply stages a panel apply and starts the oneshot unit.
// The panel never supplies health-check skips: only trusted internal
// callers (restore) stage with skipHealth.
func (b *Backend) TriggerBundleApply(ctx context.Context) (Operation, error) {
	op, err := b.stageBundleApply(ctx, nil)
	if err != nil {
		return op, err
	}
	if err := b.startStagedBundleApply(op); err != nil {
		return op, err
	}
	return op, nil
}

// stageBundleApply validates the staged apply, reserves the unique apply
// slot, writes the operation journal, and drops the tmpfs handoff (with
// the trusted skip_health bundle IDs, if any) for the oneshot unit's CLI
// run. It does NOT start the unit; the caller persists whatever state it
// needs (e.g. the apply id in a restore journal) between this and
// startStagedBundleApply. Concurrent operations are rejected before the
// handoff is replaced, so a second stage can never strand the first
// operation's journal.
func (b *Backend) stageBundleApply(ctx context.Context, skipHealth []string) (Operation, error) {
	svc := b.bundlesSvc()
	if svc == nil {
		return Operation{}, translateError(fmt.Errorf("%w: custom bundles are not configured", ErrNotConfigured))
	}
	if err := svc.EnsureRepo(ctx); err != nil {
		return Operation{}, translateError(err)
	}
	// Validate the committed merged view before reserving: a broken
	// committed bundle fails the stage here, not inside the build unit.
	merged, err := bundles.LoadMerged(ctx, b.cfg.CatalogPath, b.cfg.CustomBundlesDir)
	if err != nil {
		return Operation{}, translateError(err)
	}
	skip := normalizeSkipHealth(skipHealth)
	// A skip is only honored for bundles that actually exist in the
	// committed merged view; skipping an unknown bundle id would silently
	// mask nothing while pretending the caller's list was accepted.
	for _, id := range skip {
		if _, ok := merged.Get(id); !ok {
			return Operation{}, translateError(fmt.Errorf("%w: skip_health bundle %q is not in the merged catalog", apitypes.ErrValidation, id))
		}
	}
	op := newOperation(bundleApplyJournalKind, skip)
	op.Stage = "staged"
	op.Status = OpRunning
	if err := b.reserveBundleApply(op.ID); err != nil {
		return Operation{}, translateError(err)
	}
	if err := writeOperationJournal(b.cfg.StateDir, op); err != nil {
		b.releaseBundleApply(op.ID)
		return Operation{}, translateError(err)
	}
	if err := writeBundleApplyHandoff(op.ID, skip); err != nil {
		b.releaseBundleApply(op.ID)
		return Operation{}, translateError(err)
	}
	return op, nil
}

// startStagedBundleApply starts the omahab-bundle-apply oneshot unit for
// an operation staged by stageBundleApply. systemctl --no-block returns
// once the start job is queued, so the daemon replies promptly instead of
// blocking on the nix build inside the unit. A failed start releases the
// reservation, removes the handoff, and marks the journal failed —
// nothing is left staged.
func (b *Backend) startStagedBundleApply(op Operation) error {
	if res := b.holdBundleApply(); res == nil || res.OperationID != op.ID {
		return translateError(fmt.Errorf("%w: operation %s is not the staged bundle apply", apitypes.ErrConflict, op.ID))
	}
	out, err := execCommand("systemctl", "--no-block", "start", "omahab-bundle-apply.service")
	if err != nil {
		_ = os.Remove(bundleApplyHandoffPath())
		detail := fmt.Sprintf("start omahab-bundle-apply.service: %v: %s", err, strings.TrimSpace(out))
		op.Status = OpFailed
		op.Error = detail
		_ = writeOperationJournal(b.cfg.StateDir, op)
		b.releaseBundleApply(op.ID)
		return translateError(fmt.Errorf("%s", detail))
	}
	op.Stage = "unit-started"
	_ = writeOperationJournal(b.cfg.StateDir, op)
	return nil
}

// GetBundleApplyStatus polls one bundle apply operation by id. When the
// daemon restarted mid-apply (the switch does exactly that), the journal
// still says running until the CLI run records its outcome next to it;
// converging a terminal outcome releases the apply reservation so the
// next apply can be staged.
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
			if op.Status != OpRunning {
				b.releaseBundleApply(op.ID)
			}
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

// normalizeSkipHealth trims, deduplicates, and drops empty skip entries.
func normalizeSkipHealth(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// writeBundleApplyHandoff drops the trusted staging handoff on tmpfs for
// the oneshot unit's CLI run: the operation id plus, for internal
// restore-driven applies only, the bundle IDs whose post-switch health
// probes are skipped (restore-paused scopes mid-restore). /run is the
// daemon's declared ReadWritePaths root and is wiped at boot; the file
// holds no secret material.
func writeBundleApplyHandoff(opID string, skipHealth []string) error {
	path := bundleApplyHandoffPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create runtime dir: %w", err)
	}
	body, err := json.Marshal(struct {
		OperationID string   `json:"operation_id"`
		SkipHealth  []string `json:"skip_health,omitempty"`
	}{OperationID: opID, SkipHealth: skipHealth})
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
