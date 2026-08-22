package backups

import (
	"context"

	"github.com/omahab/omahab/internal/apps"
)

// AppLister is the narrowest view of the apps controller needed to enumerate
// backup hooks. It is satisfied by *apps.Service.
//
//   appSvc, _ := apps.NewService(db, apps.Options{...})
//   hookSrc := backups.NewAppHookSource(appSvc)
//   backupSvc := backups.New(store, cfg, backups.Deps{
//       Hooks: hookSrc,
//       ...
//   })
//
// Defining the interface in backups keeps the core backup orchestration
// (hooks.go, run.go) free of a hard import on the apps package except for
// this adapter file. If a future apps↔backups cycle appears, the integrator
// can provide a lightweight adapter that satisfies AppLister without the
// apps Service directly — the control plane then wires that adapter instead.
type AppLister interface {
	List(ctx context.Context) ([]apps.Status, error)
	CatalogBundles() []apps.Bundle
}

// AppHookSource implements HookSource by returning each installed bundle's
// Backup.PreBackup (for HookPreBackup) or Backup.PostRestore (for
// HookPostRestore) argv. Bundles without hooks for the requested kind
// contribute no entry. The adapter is the standard HookSource for the
// control plane; bundles that need no consistency simply register no hooks.
type AppHookSource struct {
	apps AppLister
}

// NewAppHookSource returns a HookSource backed by the apps service.
//
// Wiring (internal/controlplane/backend.go initServices):
//
//	catalog := ... // loaded via apps.LoadCatalogFile
//	appSvc, _ := apps.NewService(db, apps.Options{Catalog: catalog, Runner: runner, ...})
//	b.backups = backups.New(b.store, backups.Config{...}, backups.Deps{
//	    Runner:  &backups.CommandRunner{},
//	    Hooks:   backups.NewAppHookSource(appSvc),
//	    Secrets: ...,
//	    Events:  ...,
//	})
func NewAppHookSource(a AppLister) *AppHookSource {
	return &AppHookSource{apps: a}
}

var _ HookSource = (*AppHookSource)(nil)

// Hooks returns the application hooks for the given kind. Each installed
// application contributes at most one Hook whose Command is the bundle's
// Backup.PreBackup or Backup.PostRestore argv.
func (s *AppHookSource) Hooks(ctx context.Context, kind HookKind) ([]Hook, error) {
	if s == nil || s.apps == nil {
		return nil, nil
	}
	statuses, err := s.apps.List(ctx)
	if err != nil {
		return nil, err
	}
	if len(statuses) == 0 {
		return nil, nil
	}
	bundles := s.apps.CatalogBundles()
	byID := make(map[string]apps.Bundle, len(bundles))
	for _, b := range bundles {
		byID[b.ID] = b
	}
	var out []Hook
	for _, st := range statuses {
		b, ok := byID[st.BundleID]
		if !ok {
			continue
		}
		var argv []string
		switch kind {
		case HookPreBackup:
			argv = b.Backup.PreBackup
		case HookPostRestore:
			argv = b.Backup.PostRestore
		default:
			continue
		}
		if len(argv) == 0 {
			continue
		}
		// Copy to avoid aliasing the catalog slice.
		cmd := make([]string, len(argv))
		copy(cmd, argv)
		out = append(out, Hook{
			Application: b.ID,
			Command:     cmd,
		})
	}
	return out, nil
}
