package installer

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func newPackagesService(t *testing.T, probes Probes) *Service {
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

// ---------------------------------------------------------------------------
// PackagesForOS seam
// ---------------------------------------------------------------------------

func TestPackagesPackagesForOS(t *testing.T) {
	t.Parallel()

	deb, err := PackagesForOS("debian")
	if err != nil {
		t.Fatalf("debian: %v", err)
	}
	wantDeb := []string{"ca-certificates", "docker.io", "docker-compose", "nftables", "unattended-upgrades", "tailscale", "cloudflared", "podman", "uidmap", "slirp4netns", "fuse-overlayfs"}
	if !reflect.DeepEqual(deb, wantDeb) {
		t.Fatalf("debian packages = %v, want %v", deb, wantDeb)
	}

	ub, err := PackagesForOS("ubuntu")
	if err != nil {
		t.Fatalf("ubuntu: %v", err)
	}
	wantUb := []string{"ca-certificates", "docker.io", "docker-compose-v2", "nftables", "unattended-upgrades", "tailscale", "cloudflared", "podman", "uidmap", "slirp4netns", "fuse-overlayfs"}
	if !reflect.DeepEqual(ub, wantUb) {
		t.Fatalf("ubuntu packages = %v, want %v", ub, wantUb)
	}

	// case-insensitive
	if _, err := PackagesForOS("Debian"); err != nil {
		t.Fatalf("case-insensitive debian: %v", err)
	}
	if _, err := PackagesForOS("UBUNTU"); err != nil {
		t.Fatalf("case-insensitive ubuntu: %v", err)
	}

	if _, err := PackagesForOS("arch"); err == nil {
		t.Fatal("expected error for unknown OS")
	}
	if _, err := PackagesForOS(""); err == nil {
		t.Fatal("expected error for empty OS")
	}
}

// ---------------------------------------------------------------------------
// Happy path for both distros
// ---------------------------------------------------------------------------

func TestPackagesHappyPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		osID       string
		codename   string
		wantPkgs   []string
		wantTSURL  string
		wantTSSrc  string
	}{
		{
			name:      "debian trixie",
			osID:      "debian",
			codename:  "trixie",
			wantPkgs:  []string{"ca-certificates", "docker.io", "docker-compose", "nftables", "unattended-upgrades", "tailscale", "cloudflared", "podman", "uidmap", "slirp4netns", "fuse-overlayfs"},
			wantTSURL: "https://pkgs.tailscale.com/stable/debian/trixie.noarmor.gpg",
			wantTSSrc: "deb [signed-by=/usr/share/keyrings/tailscale-archive-keyring.gpg] https://pkgs.tailscale.com/stable/debian trixie main\n",
		},
		{
			name:      "ubuntu resolute",
			osID:      "ubuntu",
			codename:  "resolute",
			wantPkgs:  []string{"ca-certificates", "docker.io", "docker-compose-v2", "nftables", "unattended-upgrades", "tailscale", "cloudflared", "podman", "uidmap", "slirp4netns", "fuse-overlayfs"},
			wantTSURL: "https://pkgs.tailscale.com/stable/ubuntu/resolute.noarmor.gpg",
			wantTSSrc: "deb [signed-by=/usr/share/keyrings/tailscale-archive-keyring.gpg] https://pkgs.tailscale.com/stable/ubuntu resolute main\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			files := map[string][]byte{}
			perms := map[string]uint32{}
			var downloadCalls []struct{ url, dest string }
			var aptPkgs []string
			var refreshCalled bool
			var writes []string

			probes := Probes{
				OSRelease: func() (OSInfo, error) {
					return OSInfo{ID: tc.osID, Codename: tc.codename, VersionID: "13", Pretty: tc.osID}, nil
				},
				FileExists: func(p string) bool {
					_, ok := files[p]
					return ok
				},
				ReadFile: func(p string) ([]byte, error) {
					if b, ok := files[p]; ok {
						out := make([]byte, len(b))
						copy(out, b)
						return out, nil
					}
					return nil, os.ErrNotExist
				},
				WriteFile: func(p string, data []byte, perm uint32) error {
					cp := make([]byte, len(data))
					copy(cp, data)
					files[p] = cp
					perms[p] = perm
					writes = append(writes, p)
					return nil
				},
				DownloadFile: func(ctx context.Context, url, dest string) error {
					downloadCalls = append(downloadCalls, struct{ url, dest string }{url, dest})
					// simulate 0644 write
					files[dest] = []byte("fake-key")
					perms[dest] = 0o644
					return nil
				},
				APTRefresh: func(ctx context.Context) error {
					refreshCalled = true
					return nil
				},
				APTInstall: func(ctx context.Context, pkgs ...string) error {
					aptPkgs = append([]string(nil), pkgs...)
					return nil
				},
			}

			svc := newPackagesService(t, probes)
			res := svc.runPackagesStep(ctx, InstallOptions{})

			if res.Status != JournalCompleted {
				t.Fatalf("status = %q, error = %q, want completed", res.Status, res.Error)
			}
			if res.Step != StepPackages {
				t.Fatalf("step = %q, want %q", res.Step, StepPackages)
			}

			// Verify downloads: tailscale + cloudflare
			if len(downloadCalls) != 2 {
				t.Fatalf("download calls = %d, want 2: %v", len(downloadCalls), downloadCalls)
			}
			if downloadCalls[0].url != tc.wantTSURL || downloadCalls[0].dest != "/usr/share/keyrings/tailscale-archive-keyring.gpg" {
				t.Errorf("first download = %+v, want url %q dest tailscale keyring", downloadCalls[0], tc.wantTSURL)
			}
			if downloadCalls[1].url != "https://pkg.cloudflare.com/cloudflare-main.gpg" || downloadCalls[1].dest != "/usr/share/keyrings/cloudflare-main.gpg" {
				t.Errorf("second download = %+v, want cloudflare", downloadCalls[1])
			}

			// Verify sources
			if got := string(files["/etc/apt/sources.list.d/omahab-tailscale.list"]); got != tc.wantTSSrc {
				t.Errorf("tailscale source = %q, want %q", got, tc.wantTSSrc)
			}
			wantCloudSrc := "deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main\n"
			if got := string(files["/etc/apt/sources.list.d/omahab-cloudflared.list"]); got != wantCloudSrc {
				t.Errorf("cloudflare source = %q, want %q", got, wantCloudSrc)
			}
			if perms["/etc/apt/sources.list.d/omahab-tailscale.list"] != 0o644 {
				t.Errorf("tailscale source perm = %o, want 644", perms["/etc/apt/sources.list.d/omahab-tailscale.list"])
			}
			if perms["/etc/apt/sources.list.d/omahab-cloudflared.list"] != 0o644 {
				t.Errorf("cloudflare source perm = %o, want 644", perms["/etc/apt/sources.list.d/omahab-cloudflared.list"])
			}

			// Verify apt
			if !refreshCalled {
				t.Fatal("APTRefresh not called")
			}
			if !reflect.DeepEqual(aptPkgs, tc.wantPkgs) {
				t.Fatalf("apt pkgs = %v, want %v", aptPkgs, tc.wantPkgs)
			}

			// Verify auto-upgrades
			wantAuto := "APT::Periodic::Update-Package-Lists \"1\";\nAPT::Periodic::Unattended-Upgrade \"1\";\n"
			if got := string(files["/etc/apt/apt.conf.d/20auto-upgrades"]); got != wantAuto {
				t.Errorf("auto-upgrades = %q, want %q", got, wantAuto)
			}
			if perms["/etc/apt/apt.conf.d/20auto-upgrades"] != 0o644 {
				t.Errorf("auto-upgrades perm = %o, want 644", perms["/etc/apt/apt.conf.d/20auto-upgrades"])
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Keyring skip when present
// ---------------------------------------------------------------------------

func TestPackagesKeyringSkipWhenPresent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	files := map[string][]byte{
		"/usr/share/keyrings/tailscale-archive-keyring.gpg": []byte("existing"),
		"/usr/share/keyrings/cloudflare-main.gpg":           []byte("existing"),
	}
	var downloadCalls []string

	probes := Probes{
		OSRelease: func() (OSInfo, error) {
			return OSInfo{ID: "debian", Codename: "trixie"}, nil
		},
		FileExists: func(p string) bool {
			_, ok := files[p]
			return ok
		},
		ReadFile: func(p string) ([]byte, error) {
			if b, ok := files[p]; ok {
				return append([]byte(nil), b...), nil
			}
			return nil, os.ErrNotExist
		},
		WriteFile: func(p string, data []byte, perm uint32) error {
			files[p] = append([]byte(nil), data...)
			return nil
		},
		DownloadFile: func(ctx context.Context, url, dest string) error {
			downloadCalls = append(downloadCalls, dest)
			return nil
		},
		APTRefresh: func(ctx context.Context) error { return nil },
		APTInstall: func(ctx context.Context, pkgs ...string) error { return nil },
	}

	svc := newPackagesService(t, probes)
	res := svc.runPackagesStep(ctx, InstallOptions{})
	if res.Status != JournalCompleted {
		t.Fatalf("status = %q, error %q", res.Status, res.Error)
	}
	if len(downloadCalls) != 0 {
		t.Fatalf("downloads called despite existing keyrings: %v", downloadCalls)
	}
}

func TestPackagesKeyringDownloadWhenMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	files := map[string][]byte{}
	var downloads []struct{ url, dest string }

	probes := Probes{
		OSRelease: func() (OSInfo, error) { return OSInfo{ID: "debian", Codename: "trixie"}, nil },
		FileExists: func(p string) bool {
			_, ok := files[p]
			return ok
		},
		ReadFile: func(p string) ([]byte, error) {
			if b, ok := files[p]; ok {
				return append([]byte(nil), b...), nil
			}
			return nil, os.ErrNotExist
		},
		WriteFile: func(p string, data []byte, perm uint32) error {
			files[p] = append([]byte(nil), data...)
			return nil
		},
		DownloadFile: func(ctx context.Context, url, dest string) error {
			downloads = append(downloads, struct{ url, dest string }{url, dest})
			files[dest] = []byte("key")
			return nil
		},
		APTRefresh: func(ctx context.Context) error { return nil },
		APTInstall: func(ctx context.Context, pkgs ...string) error { return nil },
	}
	svc := newPackagesService(t, probes)
	res := svc.runPackagesStep(ctx, InstallOptions{})
	if res.Status != JournalCompleted {
		t.Fatalf("status %q err %q", res.Status, res.Error)
	}
	if len(downloads) != 2 {
		t.Fatalf("want 2 downloads, got %d", len(downloads))
	}
	// Only tailscale exists -> only cloudflare download
	files2 := map[string][]byte{
		"/usr/share/keyrings/tailscale-archive-keyring.gpg": []byte("exists"),
	}
	var downloads2 []string
	probes2 := Probes{
		OSRelease:  probes.OSRelease,
		FileExists: func(p string) bool { _, ok := files2[p]; return ok },
		ReadFile: func(p string) ([]byte, error) {
			if b, ok := files2[p]; ok {
				return append([]byte(nil), b...), nil
			}
			return nil, os.ErrNotExist
		},
		WriteFile: func(p string, data []byte, perm uint32) error {
			files2[p] = append([]byte(nil), data...)
			return nil
		},
		DownloadFile: func(ctx context.Context, url, dest string) error {
			downloads2 = append(downloads2, dest)
			files2[dest] = []byte("k")
			return nil
		},
		APTRefresh: probes.APTRefresh,
		APTInstall: probes.APTInstall,
	}
	svc2 := newPackagesService(t, probes2)
	res2 := svc2.runPackagesStep(ctx, InstallOptions{})
	if res2.Status != JournalCompleted {
		t.Fatalf("second run status %q err %q", res2.Status, res2.Error)
	}
	if len(downloads2) != 1 || downloads2[0] != "/usr/share/keyrings/cloudflare-main.gpg" {
		t.Fatalf("expected only cloudflare download, got %v", downloads2)
	}
}

// ---------------------------------------------------------------------------
// Source write skip when identical
// ---------------------------------------------------------------------------

func TestPackagesSourceWriteSkipWhenIdentical(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	debianTailscale := "deb [signed-by=/usr/share/keyrings/tailscale-archive-keyring.gpg] https://pkgs.tailscale.com/stable/debian trixie main\n"
	cloudflareSrc := "deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main\n"
	autoContent := "APT::Periodic::Update-Package-Lists \"1\";\nAPT::Periodic::Unattended-Upgrade \"1\";\n"

	files := map[string][]byte{
		"/usr/share/keyrings/tailscale-archive-keyring.gpg": []byte("k"),
		"/usr/share/keyrings/cloudflare-main.gpg":           []byte("k"),
		"/etc/apt/sources.list.d/omahab-tailscale.list":      []byte(debianTailscale),
		"/etc/apt/sources.list.d/omahab-cloudflared.list":    []byte(cloudflareSrc),
		"/etc/apt/apt.conf.d/20auto-upgrades":                []byte(autoContent),
	}
	var writeCalls []string

	probes := Probes{
		OSRelease: func() (OSInfo, error) { return OSInfo{ID: "debian", Codename: "trixie"}, nil },
		FileExists: func(p string) bool {
			_, ok := files[p]
			return ok
		},
		ReadFile: func(p string) ([]byte, error) {
			if b, ok := files[p]; ok {
				return append([]byte(nil), b...), nil
			}
			return nil, os.ErrNotExist
		},
		WriteFile: func(p string, data []byte, perm uint32) error {
			writeCalls = append(writeCalls, p)
			files[p] = append([]byte(nil), data...)
			return nil
		},
		DownloadFile: func(ctx context.Context, url, dest string) error {
			t.Fatalf("unexpected download %q", dest)
			return nil
		},
		APTRefresh: func(ctx context.Context) error { return nil },
		APTInstall: func(ctx context.Context, pkgs ...string) error { return nil },
	}

	svc := newPackagesService(t, probes)
	res := svc.runPackagesStep(ctx, InstallOptions{})
	if res.Status != JournalCompleted {
		t.Fatalf("status %q err %q", res.Status, res.Error)
	}
	if len(writeCalls) != 0 {
		t.Fatalf("expected no writes for identical sources/auto-upgrades, got %v", writeCalls)
	}
}

func TestPackagesSourceWriteWhenDiffering(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	files := map[string][]byte{
		"/usr/share/keyrings/tailscale-archive-keyring.gpg": []byte("k"),
		"/usr/share/keyrings/cloudflare-main.gpg":           []byte("k"),
		"/etc/apt/sources.list.d/omahab-tailscale.list":      []byte("old content\n"),
	}
	var writes map[string]string
	writes = map[string]string{}

	probes := Probes{
		OSRelease: func() (OSInfo, error) { return OSInfo{ID: "debian", Codename: "trixie"}, nil },
		FileExists: func(p string) bool {
			_, ok := files[p]
			return ok
		},
		ReadFile: func(p string) ([]byte, error) {
			if b, ok := files[p]; ok {
				return append([]byte(nil), b...), nil
			}
			return nil, os.ErrNotExist
		},
		WriteFile: func(p string, data []byte, perm uint32) error {
			writes[p] = string(data)
			files[p] = append([]byte(nil), data...)
			if perm != 0o644 {
				t.Errorf("perm for %q = %o, want 644", p, perm)
			}
			return nil
		},
		DownloadFile: func(ctx context.Context, url, dest string) error { return nil },
		APTRefresh: func(ctx context.Context) error { return nil },
		APTInstall: func(ctx context.Context, pkgs ...string) error { return nil },
	}

	svc := newPackagesService(t, probes)
	res := svc.runPackagesStep(ctx, InstallOptions{})
	if res.Status != JournalCompleted {
		t.Fatalf("status %q err %q", res.Status, res.Error)
	}
	if got := writes["/etc/apt/sources.list.d/omahab-tailscale.list"]; got != "deb [signed-by=/usr/share/keyrings/tailscale-archive-keyring.gpg] https://pkgs.tailscale.com/stable/debian trixie main\n" {
		t.Errorf("tailscale source not overwritten correctly, got %q", got)
	}
	// cloudflared list was missing -> should be written
	if _, ok := writes["/etc/apt/sources.list.d/omahab-cloudflared.list"]; !ok {
		t.Error("cloudflared source not written when missing")
	}
}

func TestPackagesAutoUpgradesWriteSkip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	auto := "APT::Periodic::Update-Package-Lists \"1\";\nAPT::Periodic::Unattended-Upgrade \"1\";\n"
	files := map[string][]byte{
		"/usr/share/keyrings/tailscale-archive-keyring.gpg": []byte("k"),
		"/usr/share/keyrings/cloudflare-main.gpg":           []byte("k"),
		"/etc/apt/sources.list.d/omahab-tailscale.list":      []byte("deb [signed-by=/usr/share/keyrings/tailscale-archive-keyring.gpg] https://pkgs.tailscale.com/stable/debian trixie main\n"),
		"/etc/apt/sources.list.d/omahab-cloudflared.list":    []byte("deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main\n"),
		"/etc/apt/apt.conf.d/20auto-upgrades":                []byte(auto),
	}
	var writes []string
	probes := Probes{
		OSRelease: func() (OSInfo, error) { return OSInfo{ID: "debian", Codename: "trixie"}, nil },
		FileExists: func(p string) bool { _, ok := files[p]; return ok },
		ReadFile: func(p string) ([]byte, error) {
			if b, ok := files[p]; ok {
				return append([]byte(nil), b...), nil
			}
			return nil, os.ErrNotExist
		},
		WriteFile: func(p string, data []byte, perm uint32) error {
			writes = append(writes, p)
			return nil
		},
		DownloadFile: func(ctx context.Context, url, dest string) error { return nil },
		APTRefresh: func(ctx context.Context) error { return nil },
		APTInstall: func(ctx context.Context, pkgs ...string) error { return nil },
	}
	svc := newPackagesService(t, probes)
	res := svc.runPackagesStep(ctx, InstallOptions{})
	if res.Status != JournalCompleted {
		t.Fatalf("status %q err %q", res.Status, res.Error)
	}
	for _, w := range writes {
		if w == "/etc/apt/apt.conf.d/20auto-upgrades" {
			t.Error("auto-upgrades should have been skipped when identical")
		}
	}
}

// ---------------------------------------------------------------------------
// Failure propagation
// ---------------------------------------------------------------------------

func TestPackagesFailurePropagation(t *testing.T) {
	t.Parallel()

	baseFiles := func() map[string][]byte { return map[string][]byte{} }

	makeBaseProbes := func(files map[string][]byte) Probes {
		return Probes{
			OSRelease: func() (OSInfo, error) { return OSInfo{ID: "debian", Codename: "trixie"}, nil },
			FileExists: func(p string) bool {
				_, ok := files[p]
				return ok
			},
			ReadFile: func(p string) ([]byte, error) {
				if b, ok := files[p]; ok {
					return append([]byte(nil), b...), nil
				}
				return nil, os.ErrNotExist
			},
			WriteFile: func(p string, data []byte, perm uint32) error {
				files[p] = append([]byte(nil), data...)
				return nil
			},
			DownloadFile: func(ctx context.Context, url, dest string) error {
				files[dest] = []byte("k")
				return nil
			},
			APTRefresh: func(ctx context.Context) error { return nil },
			APTInstall: func(ctx context.Context, pkgs ...string) error { return nil },
		}
	}

	t.Run("download tailscale fails", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		files := baseFiles()
		probes := makeBaseProbes(files)
		probes.DownloadFile = func(ctx context.Context, url, dest string) error {
			if strings.Contains(dest, "tailscale") {
				return errors.New("network down")
			}
			files[dest] = []byte("k")
			return nil
		}
		svc := newPackagesService(t, probes)
		res := svc.runPackagesStep(ctx, InstallOptions{})
		if res.Status != JournalFailed {
			t.Fatal("expected failed")
		}
		if !strings.Contains(strings.ToLower(res.Error), "tailscale") {
			t.Errorf("error %q should mention tailscale", res.Error)
		}
		if res.Step != StepPackages {
			t.Errorf("step %q, want packages", res.Step)
		}
	})

	t.Run("download cloudflare fails", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		files := baseFiles()
		probes := makeBaseProbes(files)
		probes.DownloadFile = func(ctx context.Context, url, dest string) error {
			if strings.Contains(dest, "cloudflare") {
				return errors.New("cloudflare key fetch failed")
			}
			files[dest] = []byte("k")
			return nil
		}
		svc := newPackagesService(t, probes)
		res := svc.runPackagesStep(ctx, InstallOptions{})
		if res.Status != JournalFailed {
			t.Fatal("expected failed")
		}
		if !strings.Contains(strings.ToLower(res.Error), "cloudflare") {
			t.Errorf("error %q should mention cloudflare", res.Error)
		}
	})

	t.Run("write source fails", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		files := baseFiles()
		probes := makeBaseProbes(files)
		probes.WriteFile = func(p string, data []byte, perm uint32) error {
			if strings.Contains(p, "omahab-tailscale") {
				return errors.New("read-only filesystem")
			}
			files[p] = append([]byte(nil), data...)
			return nil
		}
		svc := newPackagesService(t, probes)
		res := svc.runPackagesStep(ctx, InstallOptions{})
		if res.Status != JournalFailed {
			t.Fatal("expected failed")
		}
		if !strings.Contains(strings.ToLower(res.Error), "tailscale") {
			t.Errorf("error %q should mention tailscale", res.Error)
		}
	})

	t.Run("apt refresh fails", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		files := baseFiles()
		probes := makeBaseProbes(files)
		probes.APTRefresh = func(ctx context.Context) error { return errors.New("apt-get update: 404") }
		svc := newPackagesService(t, probes)
		res := svc.runPackagesStep(ctx, InstallOptions{})
		if res.Status != JournalFailed {
			t.Fatal("expected failed")
		}
		if !strings.Contains(strings.ToLower(res.Error), "apt refresh") {
			t.Errorf("error %q should mention apt refresh", res.Error)
		}
	})

	t.Run("apt install fails", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		files := baseFiles()
		probes := makeBaseProbes(files)
		probes.APTInstall = func(ctx context.Context, pkgs ...string) error { return errors.New("apt-get install: held packages") }
		svc := newPackagesService(t, probes)
		res := svc.runPackagesStep(ctx, InstallOptions{})
		if res.Status != JournalFailed {
			t.Fatal("expected failed")
		}
		if !strings.Contains(strings.ToLower(res.Error), "apt install") {
			t.Errorf("error %q should mention apt install", res.Error)
		}
	})

	t.Run("os release fails", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		files := baseFiles()
		probes := makeBaseProbes(files)
		probes.OSRelease = func() (OSInfo, error) { return OSInfo{}, errors.New("cannot read os-release") }
		svc := newPackagesService(t, probes)
		res := svc.runPackagesStep(ctx, InstallOptions{})
		if res.Status != JournalFailed {
			t.Fatal("expected failed")
		}
		if !strings.Contains(strings.ToLower(res.Error), "resolve os") && !strings.Contains(strings.ToLower(res.Error), "os") {
			t.Errorf("error %q should mention OS", res.Error)
		}
	})

	t.Run("auto-upgrades write fails", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		files := baseFiles()
		probes := makeBaseProbes(files)
		probes.WriteFile = func(p string, data []byte, perm uint32) error {
			if p == "/etc/apt/apt.conf.d/20auto-upgrades" {
				return errors.New("permission denied")
			}
			files[p] = append([]byte(nil), data...)
			return nil
		}
		svc := newPackagesService(t, probes)
		res := svc.runPackagesStep(ctx, InstallOptions{})
		if res.Status != JournalFailed {
			t.Fatal("expected failed")
		}
		if !strings.Contains(strings.ToLower(res.Error), "auto-upgrades") {
			t.Errorf("error %q should mention auto-upgrades", res.Error)
		}
	})
}

// ---------------------------------------------------------------------------
// Unknown OS rejection
// ---------------------------------------------------------------------------

func TestPackagesUnknownOS(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		info OSInfo
	}{
		{"unknown id", OSInfo{ID: "arch", Codename: "rolling", Pretty: "Arch Linux"}},
		{"debian wrong codename", OSInfo{ID: "debian", Codename: "bookworm", VersionID: "12"}},
		{"ubuntu wrong codename", OSInfo{ID: "ubuntu", Codename: "jammy", VersionID: "22.04"}},
		{"empty", OSInfo{ID: "", Codename: ""}},
		{"debian empty codename", OSInfo{ID: "debian", Codename: ""}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			probes := Probes{
				OSRelease: func() (OSInfo, error) { return tc.info, nil },
				// probes beyond OSRelease should not be called; but provide no-ops to avoid nil panic if bug
				FileExists:   func(p string) bool { return false },
				ReadFile:     func(p string) ([]byte, error) { return nil, os.ErrNotExist },
				WriteFile:    func(p string, data []byte, perm uint32) error { return nil },
				DownloadFile: func(ctx context.Context, url, dest string) error { t.Errorf("DownloadFile should not be called for unknown OS"); return nil },
				APTRefresh:   func(ctx context.Context) error { t.Errorf("APTRefresh should not be called"); return nil },
				APTInstall:   func(ctx context.Context, pkgs ...string) error { t.Errorf("APTInstall should not be called"); return nil },
			}
			svc := newPackagesService(t, probes)
			res := svc.runPackagesStep(ctx, InstallOptions{})
			if res.Status != JournalFailed {
				t.Fatalf("expected failed for %v, got %q", tc.info, res.Status)
			}
			if res.Step != StepPackages {
				t.Errorf("step %q, want packages", res.Step)
			}
			if strings.ToLower(res.Error) == "" || strings.Contains(res.Error, "TODO") || strings.Contains(res.Error, "not implemented") {
				t.Errorf("error message should be actionable, got %q", res.Error)
			}
			// error should be lowercase first letter per contract
			if len(res.Error) > 0 && res.Error[0] >= 'A' && res.Error[0] <= 'Z' {
				t.Errorf("error should be lowercase, got %q", res.Error)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Idempotent second run
// ---------------------------------------------------------------------------

func TestPackagesIdempotentSecondRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	files := map[string][]byte{}
	var downloadCount int
	var writeCounts map[string]int
	writeCounts = map[string]int{}
	var refreshCount int
	var installCount int

	probes := Probes{
		OSRelease: func() (OSInfo, error) { return OSInfo{ID: "debian", Codename: "trixie"}, nil },
		FileExists: func(p string) bool {
			_, ok := files[p]
			return ok
		},
		ReadFile: func(p string) ([]byte, error) {
			if b, ok := files[p]; ok {
				return append([]byte(nil), b...), nil
			}
			return nil, os.ErrNotExist
		},
		WriteFile: func(p string, data []byte, perm uint32) error {
			writeCounts[p]++
			files[p] = append([]byte(nil), data...)
			if perm != 0o644 {
				t.Errorf("perm for %q = %o, want 644", p, perm)
			}
			return nil
		},
		DownloadFile: func(ctx context.Context, url, dest string) error {
			downloadCount++
			files[dest] = []byte("keydata")
			return nil
		},
		APTRefresh: func(ctx context.Context) error {
			refreshCount++
			return nil
		},
		APTInstall: func(ctx context.Context, pkgs ...string) error {
			installCount++
			return nil
		},
	}

	svc := newPackagesService(t, probes)

	// First run
	res1 := svc.runPackagesStep(ctx, InstallOptions{})
	if res1.Status != JournalCompleted {
		t.Fatalf("first run status %q err %q", res1.Status, res1.Error)
	}
	if downloadCount != 2 {
		t.Fatalf("first run downloads = %d, want 2", downloadCount)
	}
	// sources + auto-upgrades = 3 writes
	if len(writeCounts) != 3 {
		t.Fatalf("first run distinct writes = %d, want 3: %v", len(writeCounts), writeCounts)
	}
	if refreshCount != 1 || installCount != 1 {
		t.Fatalf("first run refresh %d install %d, want 1 each", refreshCount, installCount)
	}

	// Reset counters but keep files map (simulating persisted FS)
	downloadCount = 0
	writeCounts = map[string]int{}
	refreshCount = 0
	installCount = 0

	// Second run should be idempotent for keyrings/sources, but apt refresh/install still happen
	res2 := svc.runPackagesStep(ctx, InstallOptions{})
	if res2.Status != JournalCompleted {
		t.Fatalf("second run status %q err %q", res2.Status, res2.Error)
	}
	if downloadCount != 0 {
		t.Fatalf("second run downloads = %d, want 0 (idempotent)", downloadCount)
	}
	if len(writeCounts) != 0 {
		t.Fatalf("second run writes = %v, want 0 (all identical)", writeCounts)
	}
	// Apt is expected to run again (install is idempotent but not skipped)
	if refreshCount != 1 {
		t.Errorf("second run refresh %d, want 1", refreshCount)
	}
	if installCount != 1 {
		t.Errorf("second run install %d, want 1", installCount)
	}

	// Verify final file contents are still correct
	wantTailscale := "deb [signed-by=/usr/share/keyrings/tailscale-archive-keyring.gpg] https://pkgs.tailscale.com/stable/debian trixie main\n"
	if got := string(files["/etc/apt/sources.list.d/omahab-tailscale.list"]); got != wantTailscale {
		t.Errorf("tailscale source after second run = %q, want %q", got, wantTailscale)
	}
	wantAuto := "APT::Periodic::Update-Package-Lists \"1\";\nAPT::Periodic::Unattended-Upgrade \"1\";\n"
	if got := string(files["/etc/apt/apt.conf.d/20auto-upgrades"]); got != wantAuto {
		t.Errorf("auto-upgrades after second run = %q, want %q", got, wantAuto)
	}
}

// ---------------------------------------------------------------------------
// Nil probe safety
// ---------------------------------------------------------------------------

func TestPackagesNilProbes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cases := []struct {
		name   string
		probes Probes
	}{
		{
			name:   "nil OSRelease",
			probes: Probes{ /* all nil */ },
		},
		{
			name: "nil FileExists",
			probes: Probes{
				OSRelease: func() (OSInfo, error) { return OSInfo{ID: "debian", Codename: "trixie"}, nil },
			},
		},
		{
			name: "nil DownloadFile",
			probes: Probes{
				OSRelease:  func() (OSInfo, error) { return OSInfo{ID: "debian", Codename: "trixie"}, nil },
				FileExists: func(p string) bool { return false },
			},
		},
		{
			name: "nil ReadFile",
			probes: Probes{
				OSRelease:    func() (OSInfo, error) { return OSInfo{ID: "debian", Codename: "trixie"}, nil },
				FileExists:   func(p string) bool { return false },
				DownloadFile: func(ctx context.Context, url, dest string) error { return nil },
			},
		},
		{
			name: "nil WriteFile",
			probes: Probes{
				OSRelease:    func() (OSInfo, error) { return OSInfo{ID: "debian", Codename: "trixie"}, nil },
				FileExists:   func(p string) bool { return false },
				DownloadFile: func(ctx context.Context, url, dest string) error { return nil },
				ReadFile:     func(p string) ([]byte, error) { return nil, os.ErrNotExist },
			},
		},
		{
			name: "nil APTRefresh",
			probes: Probes{
				OSRelease:    func() (OSInfo, error) { return OSInfo{ID: "debian", Codename: "trixie"}, nil },
				FileExists:   func(p string) bool { return true },
				DownloadFile: func(ctx context.Context, url, dest string) error { return nil },
				ReadFile:     func(p string) ([]byte, error) { return nil, os.ErrNotExist },
				WriteFile:    func(p string, data []byte, perm uint32) error { return nil },
			},
		},
		{
			name: "nil APTInstall",
			probes: Probes{
				OSRelease:    func() (OSInfo, error) { return OSInfo{ID: "debian", Codename: "trixie"}, nil },
				FileExists:   func(p string) bool { return true },
				DownloadFile: func(ctx context.Context, url, dest string) error { return nil },
				ReadFile:     func(p string) ([]byte, error) { return nil, os.ErrNotExist },
				WriteFile:    func(p string, data []byte, perm uint32) error { return nil },
				APTRefresh:   func(ctx context.Context) error { return nil },
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := newPackagesService(t, tc.probes)
			// Should not panic
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic with nil probe %q: %v", tc.name, r)
				}
			}()
			res := svc.runPackagesStep(ctx, InstallOptions{})
			if res.Status != JournalFailed {
				t.Fatalf("expected failed for %q, got %q", tc.name, res.Status)
			}
			if res.Error == "" {
				t.Error("expected non-empty error")
			}
			if res.Step != StepPackages {
				t.Errorf("step = %q, want packages", res.Step)
			}
		})
	}
}

func TestPackagesRollbackIsNoop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if err := RollbackPackages(ctx, Probes{}); err != nil {
		t.Fatalf("RollbackPackages should return nil, got %v", err)
	}
	// With nil probes and context
	if err := RollbackPackages(ctx, Probes{APTRefresh: func(ctx context.Context) error { return errors.New("should not be called") }}); err != nil {
		t.Fatalf("RollbackPackages should remain nil, got %v", err)
	}
}
