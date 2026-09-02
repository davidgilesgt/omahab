package apps

import (
	"context"

	"github.com/omahab/omahab/internal/domain"
)

// RuntimeRouter dispatches Runner operations to the runner matching the
// bundle's declared runtime. Bundle placement lives in the catalog
// ("runtime": "systemd" | "compose"); the router resolves the bundle for
// each application and forwards to the matching runner.
type RuntimeRouter struct {
	catalog *Catalog
	systemd *SystemdRunner
	compose *ComposeRunner
}

// NewRuntimeRouter builds a dispatching runner. Catalog is required.
func NewRuntimeRouter(catalog *Catalog, systemd *SystemdRunner, compose *ComposeRunner) (*RuntimeRouter, error) {
	if catalog == nil {
		return nil, invalid("catalog is required")
	}
	if systemd == nil {
		return nil, invalid("systemd runner is required")
	}
	if compose == nil {
		return nil, invalid("compose runner is required")
	}
	return &RuntimeRouter{catalog: catalog, systemd: systemd, compose: compose}, nil
}

func (r *RuntimeRouter) runnerFor(bundleID string) (Runner, error) {
	bundle, ok := r.catalog.Get(bundleID)
	if !ok {
		// Unknown bundle: default to compose so errors surface from the
		// compose path rather than a confusing "no units" error.
		return r.compose, nil
	}
	if bundle.Runtime == RuntimeSystemd {
		return r.systemd, nil
	}
	return r.compose, nil
}

func (r *RuntimeRouter) Deploy(ctx context.Context, app domain.Application, spec DeploySpec) error {
	runner, err := r.runnerFor(app.BundleID)
	if err != nil {
		return err
	}
	return runner.Deploy(ctx, app, spec)
}

func (r *RuntimeRouter) Start(ctx context.Context, app domain.Application, spec DeploySpec) error {
	runner, err := r.runnerFor(app.BundleID)
	if err != nil {
		return err
	}
	return runner.Start(ctx, app, spec)
}

func (r *RuntimeRouter) Stop(ctx context.Context, app domain.Application, spec DeploySpec) error {
	runner, err := r.runnerFor(app.BundleID)
	if err != nil {
		return err
	}
	return runner.Stop(ctx, app, spec)
}

func (r *RuntimeRouter) Remove(ctx context.Context, app domain.Application, spec DeploySpec) error {
	runner, err := r.runnerFor(app.BundleID)
	if err != nil {
		return err
	}
	return runner.Remove(ctx, app, spec)
}

func (r *RuntimeRouter) Check(ctx context.Context, app domain.Application, spec DeploySpec) (domain.Health, error) {
	runner, err := r.runnerFor(app.BundleID)
	if err != nil {
		return domain.HealthUnknown, err
	}
	return runner.Check(ctx, app, spec)
}

var _ Runner = (*RuntimeRouter)(nil)
