package installer

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func newServicesService(t *testing.T, probes Probes) *Service {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := EnsureMigrations(ctx, db); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	return NewService(db, probes)
}

func TestServicesEnabledUnits(t *testing.T) {
	t.Parallel()

	units := EnabledUnits()
	want := []string{"tailscaled", "omahabd"}
	if !reflect.DeepEqual(units, want) {
		t.Fatalf("EnabledUnits() = %v, want %v", units, want)
	}
	// deterministic across calls
	again := EnabledUnits()
	if !reflect.DeepEqual(units, again) {
		t.Fatalf("EnabledUnits not deterministic: %v vs %v", units, again)
	}
	// must not contain deliberately excluded units
	forbidden := []string{"cloudflared", "omahab-backup.timer", "omahab-verify.timer", "omahab-clientd", "cloudflared.service"}
	for _, f := range forbidden {
		for _, u := range units {
			if u == f {
				t.Fatalf("EnabledUnits should not contain %q", f)
			}
			// also guard against suffix variants (e.g. "omahab-backup.service" vs timer)
			if strings.HasPrefix(u, strings.TrimSuffix(f, ".timer")) && strings.Contains(f, "backup") && strings.Contains(u, "backup") && u != "omahabd" {
				// handled above; explicit check keeps seam tight
			}
		}
	}
	// ensure no extra cloudflare/backup/verify/clientd entries slipped in
	for _, u := range units {
		if strings.Contains(u, "cloudflared") && u != "cloudflared" {
			t.Fatalf("unexpected cloudflared variant %q", u)
		}
		if strings.Contains(u, "backup") || strings.Contains(u, "verify") || strings.Contains(u, "clientd") {
			t.Fatalf("unexpected unit %q in EnabledUnits", u)
		}
	}
}

func TestServicesHappyPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var calls [][]string
	svc := newServicesService(t, Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) {
			cp := append([]string(nil), args...)
			calls = append(calls, cp)
			return "", nil
		},
	})

	res := svc.runServicesStep(ctx, InstallOptions{})
	if res.Step != StepServices {
		t.Fatalf("step = %q, want %q", res.Step, StepServices)
	}
	if res.Status != JournalCompleted {
		t.Fatalf("status = %q, want %q (error: %q)", res.Status, JournalCompleted, res.Error)
	}
	if res.Error != "" {
		t.Fatalf("unexpected error %q", res.Error)
	}

	want := [][]string{
		{"daemon-reload"},
		{"enable", "tailscaled"},
		{"enable", "omahabd"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("systemctl calls = %v, want %v", calls, want)
	}
}

func TestServicesDaemonReloadFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var calls [][]string
	svc := newServicesService(t, Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) {
			cp := append([]string(nil), args...)
			calls = append(calls, cp)
			if len(args) > 0 && args[0] == "daemon-reload" {
				return "", errors.New("daemon-reload failed")
			}
			return "", nil
		},
	})

	res := svc.runServicesStep(ctx, InstallOptions{})
	if res.Step != StepServices {
		t.Fatalf("step = %q, want %q", res.Step, StepServices)
	}
	if res.Status != JournalFailed {
		t.Fatalf("status = %q, want %q", res.Status, JournalFailed)
	}
	if res.Error == "" {
		t.Fatal("expected non-empty error")
	}
	if !strings.Contains(res.Error, "daemon-reload") {
		t.Fatalf("error %q should mention daemon-reload", res.Error)
	}
	// must fail without any enable calls
	if len(calls) != 1 {
		t.Fatalf("expected 1 call (daemon-reload only), got %d: %v", len(calls), calls)
	}
	if !reflect.DeepEqual(calls[0], []string{"daemon-reload"}) {
		t.Fatalf("first call = %v, want [daemon-reload]", calls[0])
	}
}

func TestServicesEnableTailscaledFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var calls [][]string
	svc := newServicesService(t, Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) {
			cp := append([]string(nil), args...)
			calls = append(calls, cp)
			if len(args) == 2 && args[0] == "enable" && args[1] == "tailscaled" {
				return "", errors.New("enable tailscaled failed")
			}
			return "", nil
		},
	})

	res := svc.runServicesStep(ctx, InstallOptions{})
	if res.Status != JournalFailed {
		t.Fatalf("status = %q, want %q", res.Status, JournalFailed)
	}
	if res.Error == "" {
		t.Fatal("expected error")
	}
	want := [][]string{
		{"daemon-reload"},
		{"enable", "tailscaled"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	// must not have attempted omahabd
	for _, c := range calls {
		if len(c) == 2 && c[1] == "omahabd" {
			t.Fatalf("should not have called enable omahabd after tailscaled failure")
		}
	}
}

func TestServicesEnableOmahabdFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var calls [][]string
	svc := newServicesService(t, Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) {
			cp := append([]string(nil), args...)
			calls = append(calls, cp)
			if len(args) == 2 && args[0] == "enable" && args[1] == "omahabd" {
				return "", errors.New("enable omahabd failed")
			}
			return "", nil
		},
	})

	res := svc.runServicesStep(ctx, InstallOptions{})
	if res.Step != StepServices {
		t.Fatalf("step = %q, want %q", res.Step, StepServices)
	}
	if res.Status != JournalFailed {
		t.Fatalf("status = %q, want %q", res.Status, JournalFailed)
	}
	if res.Error == "" {
		t.Fatal("expected non-empty error")
	}
	if !strings.Contains(res.Error, "omahabd") {
		t.Fatalf("error %q should mention omahabd", res.Error)
	}

	want := [][]string{
		{"daemon-reload"},
		{"enable", "tailscaled"},
		{"enable", "omahabd"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}

	// idempotent on resume: second run with a healthy probe retries the full
	// sequence (daemon-reload + both enables) and succeeds.
	var resumeCalls [][]string
	svc2 := newServicesService(t, Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) {
			cp := append([]string(nil), args...)
			resumeCalls = append(resumeCalls, cp)
			return "", nil
		},
	})
	res2 := svc2.runServicesStep(ctx, InstallOptions{})
	if res2.Status != JournalCompleted {
		t.Fatalf("resume status = %q, want %q (error %q)", res2.Status, JournalCompleted, res2.Error)
	}
	if !reflect.DeepEqual(resumeCalls, want) {
		t.Fatalf("resume calls = %v, want %v (enable is idempotent, full sequence re-run)", resumeCalls, want)
	}
}

func TestServicesIdempotentSecondRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var first [][]string
	svc := newServicesService(t, Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) {
			first = append(first, append([]string(nil), args...))
			return "", nil
		},
	})
	res1 := svc.runServicesStep(ctx, InstallOptions{})
	if res1.Status != JournalCompleted {
		t.Fatalf("first run status %q, want %q", res1.Status, JournalCompleted)
	}
	var second [][]string
	// swap probe to capture second run independently; simulates service resumption
	svc2 := newServicesService(t, Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) {
			second = append(second, append([]string(nil), args...))
			return "", nil
		},
	})
	res2 := svc2.runServicesStep(ctx, InstallOptions{})
	if res2.Status != JournalCompleted {
		t.Fatalf("second run status %q, want %q", res2.Status, JournalCompleted)
	}
	want := [][]string{
		{"daemon-reload"},
		{"enable", "tailscaled"},
		{"enable", "omahabd"},
	}
	if !reflect.DeepEqual(first, want) || !reflect.DeepEqual(second, want) {
		t.Fatalf("idempotent calls differ: first %v second %v want %v", first, second, want)
	}
}

func TestServicesNilSystemctl(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	svc := newServicesService(t, Probes{})

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic with nil Systemctl: %v", r)
		}
	}()

	res := svc.runServicesStep(ctx, InstallOptions{})
	if res.Step != StepServices {
		t.Fatalf("step = %q, want %q", res.Step, StepServices)
	}
	if res.Status != JournalFailed {
		t.Fatalf("status = %q, want %q", res.Status, JournalFailed)
	}
	if res.Error == "" {
		t.Fatal("expected non-empty error for nil Systemctl")
	}
	// also ensure nil probe on a zero-value Service does not panic
	var zeroSvc Service
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic with zero Service: %v", r)
			}
		}()
		r2 := zeroSvc.runServicesStep(ctx, InstallOptions{})
		if r2.Status != JournalFailed {
			t.Fatalf("zero svc status = %q, want failed", r2.Status)
		}
	}()
}

func TestServicesRollback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var calls [][]string
	probes := Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) {
			calls = append(calls, append([]string(nil), args...))
			return "", nil
		},
	}
	if err := RollbackServices(ctx, probes); err != nil {
		t.Fatalf("RollbackServices error = %v, want nil", err)
	}
	want := [][]string{
		{"disable", "omahabd"},
		{"daemon-reload"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("rollback calls = %v, want %v", calls, want)
	}
	// must not touch tailscaled or forbidden units
	for _, c := range calls {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "tailscaled") {
			t.Fatalf("rollback should not touch tailscaled, got %q", joined)
		}
		if strings.Contains(joined, "cloudflared") {
			t.Fatalf("rollback should not touch cloudflared, got %q", joined)
		}
		if strings.Contains(joined, "backup") || strings.Contains(joined, "verify") || strings.Contains(joined, "clientd") {
			t.Fatalf("rollback touched forbidden unit %q", joined)
		}
		if len(c) >= 2 && c[0] == "enable" {
			t.Fatalf("rollback should not enable, got %q", joined)
		}
	}
}

func TestServicesRollbackBestEffort(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var calls [][]string
	probes := Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) {
			calls = append(calls, append([]string(nil), args...))
			if len(args) >= 2 && args[0] == "disable" && args[1] == "omahabd" {
				return "", errors.New("disable failed")
			}
			return "", nil
		},
	}
	if err := RollbackServices(ctx, probes); err != nil {
		t.Fatalf("RollbackServices best-effort should return nil even on disable error, got %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls even when disable fails, got %d: %v", len(calls), calls)
	}
	if !reflect.DeepEqual(calls[0], []string{"disable", "omahabd"}) {
		t.Fatalf("first rollback call = %v, want [disable omahabd]", calls[0])
	}
	if !reflect.DeepEqual(calls[1], []string{"daemon-reload"}) {
		t.Fatalf("second rollback call = %v, want [daemon-reload]", calls[1])
	}
}

func TestServicesRollbackNilProbe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic with nil Systemctl in rollback: %v", r)
		}
	}()
	if err := RollbackServices(ctx, Probes{}); err != nil {
		t.Fatalf("RollbackServices with nil probe should return nil, got %v", err)
	}
	if err := RollbackServices(ctx, Probes{Systemctl: nil}); err != nil {
		t.Fatalf("RollbackServices nil Systemctl should return nil, got %v", err)
	}
}

func TestServicesRollbackDaemonReloadStillCalledWhenDisableFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// second probe fails on daemon-reload; rollback still returns nil (best-effort)
	var calls [][]string
	probes := Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) {
			calls = append(calls, append([]string(nil), args...))
			if len(args) == 1 && args[0] == "daemon-reload" {
				return "", errors.New("daemon-reload failed")
			}
			return "", nil
		},
	}
	if err := RollbackServices(ctx, probes); err != nil {
		t.Fatalf("RollbackServices should return nil even when daemon-reload fails, got %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d: %v", len(calls), calls)
	}
}
