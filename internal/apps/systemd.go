package apps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/omahab/omahab/internal/domain"
)

// DefaultAppEnvDir is where per-bundle env files live for natively placed
// bundles. omahabd renders these files; systemd units gate on their
// existence (ConditionPathExists) so units stay cleanly skipped before
// enrollment.
const DefaultAppEnvDir = "/var/lib/omahab/appenv"

// SystemdRunner implements Runner for natively placed bundles (NixOS
// systemd services). Deploy writes the bundle's env file (from the
// EnvSource projection carried in spec.Env) and restarts the unit set;
// Start/Stop drive systemctl; Remove stops the units — native apps are
// never uninstalled, the system closure defines them.
type SystemdRunner struct {
	invoker Invoker
	envDir  string
	// units maps bundle ID -> systemd units to control.
	units func(bundleID string) []string
}

// NewSystemdRunner builds a systemd runner. invoker nil means ExecInvoker;
// envDir empty means DefaultAppEnvDir. The units resolver must come from
// the catalog; nil means derive from bundle ID (bundle.service).
func NewSystemdRunner(invoker Invoker, envDir string, units func(bundleID string) []string) *SystemdRunner {
	if invoker == nil {
		invoker = ExecInvoker{}
	}
	if envDir == "" {
		envDir = DefaultAppEnvDir
	}
	if units == nil {
		units = defaultUnitsFor
	}
	return &SystemdRunner{invoker: invoker, envDir: envDir, units: units}
}

func defaultUnitsFor(bundleID string) []string {
	return []string{bundleID + ".service"}
}

func (r *SystemdRunner) envPath(app domain.Application) string {
	return filepath.Join(r.envDir, app.BundleID+".env")
}

// writeEnvFile atomically writes the env projection as KEY=VALUE lines
// (0600). This is the file the nix-defined units consume via
// EnvironmentFile and gate on via ConditionPathExists.
func (r *SystemdRunner) writeEnvFile(app domain.Application, spec DeploySpec) error {
	if err := os.MkdirAll(r.envDir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", r.envDir, err)
	}
	var b strings.Builder
	for _, kv := range spec.Env {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		b.WriteString(kv)
		b.WriteByte('\n')
	}
	path := r.envPath(app)
	tmp, err := os.CreateTemp(r.envDir, "."+app.BundleID+".env-*")
	if err != nil {
		return fmt.Errorf("create temp env: %w", err)
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func (r *SystemdRunner) systemctl(ctx context.Context, verb string, args ...string) error {
	var out strings.Builder
	cmd := append([]string{verb}, args...)
	if err := r.invoker.Run(ctx, nil, "", nil, &out, "systemctl", cmd...); err != nil {
		return fmt.Errorf("systemctl %s %s: %v: %s", verb, strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return nil
}

// Deploy writes the env file then restarts the units.
func (r *SystemdRunner) Deploy(ctx context.Context, app domain.Application, spec DeploySpec) error {
	if err := r.writeEnvFile(app, spec); err != nil {
		return err
	}
	units := r.units(app.BundleID)
	if len(units) == 0 {
		return fmt.Errorf("systemd runner: no units declared for bundle %q", app.BundleID)
	}
	return r.systemctl(ctx, "restart", units...)
}

func (r *SystemdRunner) Start(ctx context.Context, app domain.Application, spec DeploySpec) error {
	units := r.units(app.BundleID)
	if len(units) == 0 {
		return fmt.Errorf("systemd runner: no units declared for bundle %q", app.BundleID)
	}
	return r.systemctl(ctx, "start", units...)
}

func (r *SystemdRunner) Stop(ctx context.Context, app domain.Application, spec DeploySpec) error {
	units := r.units(app.BundleID)
	if len(units) == 0 {
		return fmt.Errorf("systemd runner: no units declared for bundle %q", app.BundleID)
	}
	return r.systemctl(ctx, "stop", units...)
}

// Remove stops the units. Native bundles cannot be uninstalled — the
// system closure defines them — so Remove is a stop, and the catalog
// service refuses Uninstall for native bundles earlier in the pipeline.
func (r *SystemdRunner) Remove(ctx context.Context, app domain.Application, spec DeploySpec) error {
	return r.Stop(ctx, app, spec)
}

// Check observes health for native bundles: HTTP probes hit the loopback
// port from the catalog health check; command checks are unsupported
// (there is no container to exec inside) and report unknown.
func (r *SystemdRunner) Check(ctx context.Context, app domain.Application, spec DeploySpec) (domain.Health, error) {
	switch spec.Health.Kind {
	case CheckHTTP:
		return probeHTTP(ctx, spec.Health, spec.Health.Port)
	case CheckCommand:
		// No container to exec in: native services are probed via HTTP
		// or not at all. Report unknown rather than failing.
		return domain.HealthUnknown, nil
	default:
		return domain.HealthUnknown, nil
	}
}
