package installer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// passingProbes returns a Probes set that makes every check pass or warn (no fails).
// Individual tests override specific fields to trigger targeted failures.
func passingProbes() Probes {
	return Probes{
		OSRelease: func() (OSInfo, error) {
			return OSInfo{ID: "debian", VersionID: "13", Codename: "trixie", Pretty: "Debian GNU/Linux 13 (trixie)"}, nil
		},
		Arch: func() (string, error) { return "amd64", nil },
		DirExists: func(p string) bool {
			switch p {
			case "/run/systemd/system":
				return true
			case "/srv/omahab", "/var/lib/omahab":
				return false
			default:
				return false
			}
		},
		DirNotEmpty: func(p string) (bool, error) { return false, nil },
		CommandExists: func(name string) bool {
			// apt-get present so package check passes; container runtimes absent
			if name == "apt-get" {
				return true
			}
			return false
		},
		CommandOutput: func(ctx context.Context, name string, args ...string) (string, error) {
			return "", nil
		},
		RunningPids: func() ([]int, error) { return nil, nil },
		ProcessCmdline: func(pid int) (string, error) { return "", nil },
		ListeningPorts: func() ([]int, error) { return nil, nil },
		ServiceActive: func(name string) (bool, error) { return false, nil },
		APTSources: func() ([]AptSource, error) {
			// debian archive only — ignored by check
			return []AptSource{
				{File: "/etc/apt/sources.list", Line: "deb https://deb.debian.org/debian trixie main"},
			}, nil
		},
		MemInfo: func() (MemInfo, error) { return MemInfo{Total: 4 * 1024 * 1024 * 1024}, nil },
		DiskInfo: func(p string) (DiskInfo, error) {
			return DiskInfo{Total: 30 * 1024 * 1024 * 1024, Free: 10 * 1024 * 1024 * 1024}, nil
		},
		FileOwner: func(p string) (int, int, error) { return 0, 0, nil },
		Now: func() time.Time { return time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC) },
		DNSLookup: func(ctx context.Context, host string) ([]string, error) {
			return []string{"1.2.3.4"}, nil
		},
		HTTPSGet: func(ctx context.Context, url string) (int, []byte, error) {
			return 200, []byte("ok"), nil
		},
		AuthorizedKeys: func(user string) (string, []string, error) {
			if user == "root" {
				return "/root/.ssh/authorized_keys", []string{"ssh-ed25519 AAAAC3test root@host"}, nil
			}
			return "", nil, nil
		},
		ActiveSSHSession: func() (bool, string, error) { return true, "1.2.3.4", nil },
	}
}

func findCheck(results []CheckResult, name string) *CheckResult {
	for i := range results {
		if results[i].Name == name {
			return &results[i]
		}
	}
	return nil
}

func TestPreflightCleanHost(t *testing.T) {
	ctx := context.Background()
	probes := passingProbes()
	results, err := RunPreflight(ctx, probes)
	if err != nil {
		t.Fatalf("expected clean host to pass, got err %v results=%+v", err, results)
	}
	for _, r := range results {
		if r.Level == LevelFail {
			t.Fatalf("unexpected fail: %q level=%s msg=%q", r.Name, r.Level, r.Message)
		}
	}
	// sanity: ensure systemd and apt_sources are present and passing
	if c := findCheck(results, "systemd"); c == nil || c.Level != LevelPass {
		t.Fatalf("expected systemd pass, got %+v", c)
	}
	if c := findCheck(results, "apt_sources"); c == nil || c.Level != LevelPass {
		t.Fatalf("expected apt_sources pass, got %+v", c)
	}
}

func TestPreflightCheckOrder(t *testing.T) {
	ctx := context.Background()
	probes := passingProbes()
	results, _ := RunPreflight(ctx, probes)
	want := []string{"os", "arch", "systemd", "containers", "data_dir", "ports", "services", "apt_sources", "ram", "disk", "filesystem", "time", "dns", "https", "ssh_keys", "packages"}
	if len(results) != len(want) {
		t.Fatalf("expected %d checks, got %d: %v", len(want), len(results), func() []string {
			var n []string
			for _, r := range results {
				n = append(n, r.Name)
			}
			return n
		}())
	}
	for i, w := range want {
		if results[i].Name != w {
			t.Fatalf("order mismatch at index %d: want %q got %q", i, w, results[i].Name)
		}
	}
}

func TestPreflightSystemdMissing(t *testing.T) {
	ctx := context.Background()
	probes := passingProbes()
	// override DirExists to report systemd missing
	orig := probes.DirExists
	probes.DirExists = func(p string) bool {
		if p == "/run/systemd/system" {
			return false
		}
		return orig(p)
	}
	results, err := RunPreflight(ctx, probes)
	if err == nil {
		t.Fatalf("expected fail when systemd missing")
	}
	c := findCheck(results, "systemd")
	if c == nil {
		t.Fatal("systemd check not found")
	}
	if c.Level != LevelFail {
		t.Fatalf("expected systemd fail, got %s msg=%q", c.Level, c.Message)
	}
	if c.Dirty {
		t.Fatalf("systemd fail should not be dirty (container is wrong host, not compromised)")
	}
	if !strings.Contains(c.Message, "/run/systemd/system") {
		t.Fatalf("systemd message must name missing marker, got %q", c.Message)
	}
	if !strings.Contains(c.Remediation, "install on a fresh Debian 13 or Ubuntu 26.04 system booted with systemd; containers and chroots are not supported") {
		t.Fatalf("systemd remediation mismatch, got %q", c.Remediation)
	}
	// also ensure PreflightError reports non-dirty for this alone failure?
	// When only systemd fails, IsDirty should be false.
	if pe, ok := err.(*PreflightError); ok {
		if pe.IsDirty() {
			t.Fatalf("systemd-only failure should not be dirty")
		}
	}
}

func TestPreflightSystemdNilProbe(t *testing.T) {
	ctx := context.Background()
	probes := passingProbes()
	probes.DirExists = nil
	results, err := RunPreflight(ctx, probes)
	if err != nil {
		t.Fatalf("nil DirExists should not cause overall fail (warn only), got err %v", err)
	}
	c := findCheck(results, "systemd")
	if c == nil {
		t.Fatal("systemd check not found")
	}
	if c.Level != LevelWarn {
		t.Fatalf("expected systemd warn on nil probe, got %s", c.Level)
	}
}

func TestPreflightAPTSources(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name      string
		sources   []AptSource
		wantLevel Level
		wantDirty bool
		wantMsg   string // substring that must appear when pass
		wantFail  bool
	}{
		{
			name: "omahab vendor tailscale+cloudflared both allowlisted pass",
			sources: []AptSource{
				{File: "/etc/apt/sources.list.d/omahab-tailscale.list", Line: "deb [signed-by=/usr/share/keyrings/tailscale-archive-keyring.gpg] https://pkgs.tailscale.com/stable/debian trixie main"},
				{File: "/etc/apt/sources.list.d/omahab-cloudflared.list", Line: "deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main"},
			},
			wantLevel: LevelPass,
			wantMsg:   "omahab-managed vendor source",
		},
		{
			name: "omahab-tailscale alone passes",
			sources: []AptSource{
				{File: "/etc/apt/sources.list.d/omahab-tailscale.list", Line: "deb [signed-by=/usr/share/keyrings/tailscale-archive-keyring.gpg] https://pkgs.tailscale.com/stable/debian trixie main"},
			},
			wantLevel: LevelPass,
			wantMsg:   "omahab-managed vendor source (tailscale)",
		},
		{
			name: "omahab-cloudflared alone passes",
			sources: []AptSource{
				{File: "/etc/apt/sources.list.d/omahab-cloudflared.list", Line: "deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main"},
			},
			wantLevel: LevelPass,
			wantMsg:   "omahab-managed vendor source (cloudflare)",
		},
		{
			name: "omahab-evil.list with random host fails dirty",
			sources: []AptSource{
				{File: "/etc/apt/sources.list.d/omahab-evil.list", Line: "deb https://evil.example.com/debian stable main"},
			},
			wantLevel: LevelFail,
			wantDirty: true,
			wantFail:  true,
		},
		{
			name: "omahab-tailscale.list with non-vendor host fails dirty",
			sources: []AptSource{
				{File: "/etc/apt/sources.list.d/omahab-tailscale.list", Line: "deb https://evil.example.com/debian stable main"},
			},
			wantLevel: LevelFail,
			wantDirty: true,
			wantFail:  true,
		},
		{
			name: "non-omahab file with vendor host fails dirty",
			sources: []AptSource{
				{File: "/etc/apt/sources.list.d/custom.list", Line: "deb https://pkgs.tailscale.com/stable/debian trixie main"},
			},
			wantLevel: LevelFail,
			wantDirty: true,
			wantFail:  true,
		},
		{
			name: "non-omahab file with cloudflare host fails dirty",
			sources: []AptSource{
				{File: "/etc/apt/sources.list.d/extra.list", Line: "deb https://pkg.cloudflare.com/cloudflared any main"},
			},
			wantLevel: LevelFail,
			wantDirty: true,
			wantFail:  true,
		},
		{
			name: "debian archive still ignored",
			sources: []AptSource{
				{File: "/etc/apt/sources.list", Line: "deb https://deb.debian.org/debian trixie main"},
				{File: "/etc/apt/sources.list.d/omahab-tailscale.list", Line: "deb [signed-by=/usr/share/keyrings/tailscale-archive-keyring.gpg] https://pkgs.tailscale.com/stable/debian trixie main"},
			},
			wantLevel: LevelPass,
			wantMsg:   "omahab-managed vendor source",
		},
		{
			name: "vendor plus evil fails dirty",
			sources: []AptSource{
				{File: "/etc/apt/sources.list.d/omahab-tailscale.list", Line: "deb https://pkgs.tailscale.com/stable/debian trixie main"},
				{File: "/etc/apt/sources.list.d/evil.list", Line: "deb https://evil.example.com/debian stable main"},
			},
			wantLevel: LevelFail,
			wantDirty: true,
			wantFail:  true,
		},
		{
			name: "sources.list omahab file not under sources.list.d fails",
			sources: []AptSource{
				{File: "/etc/apt/sources.list", Line: "deb https://pkgs.tailscale.com/stable/debian trixie main"},
			},
			wantLevel: LevelFail,
			wantDirty: true,
			wantFail:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			probes := passingProbes()
			probes.APTSources = func() ([]AptSource, error) { return tc.sources, nil }
			results, err := RunPreflight(ctx, probes)
			c := findCheck(results, "apt_sources")
			if c == nil {
				t.Fatal("apt_sources check not found")
			}
			if c.Level != tc.wantLevel {
				t.Fatalf("want level %s got %s msg=%q results=%+v", tc.wantLevel, c.Level, c.Message, results)
			}
			if tc.wantFail && err == nil {
				t.Fatalf("expected RunPreflight to return error on fail")
			}
			if !tc.wantFail && err != nil {
				// if only apt_sources passes and others pass, err should be nil
				t.Fatalf("unexpected error: %v", err)
			}
			if c.Dirty != tc.wantDirty {
				t.Fatalf("want dirty=%v got %v", tc.wantDirty, c.Dirty)
			}
			if tc.wantMsg != "" && !strings.Contains(c.Message, tc.wantMsg) {
				t.Fatalf("expected message to contain %q got %q", tc.wantMsg, c.Message)
			}
		})
	}
}
func TestPreflightContainers(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name       string
		out        string
		err        error
		wantLevel  Level
		wantDirty  bool
		wantMsg    string
		wantRemed  string
	}{
		{
			name:      "docker ps error daemon not reachable fails not dirty",
			out:       "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?",
			err:       errors.New("exit status 1"),
			wantLevel: LevelFail,
			wantDirty: false,
			wantMsg:   "docker is installed but its daemon is not reachable",
			wantRemed: "start the daemon (systemctl start docker) so preflight can list containers, or remove docker",
		},
		{
			name:      "docker ps ok with ids fails dirty",
			out:       "abc123\ndef456\n",
			err:       nil,
			wantLevel: LevelFail,
			wantDirty: true,
			wantMsg:   "existing Docker containers found",
		},
		{
			name:      "docker ps ok empty passes",
			out:       "",
			err:       nil,
			wantLevel: LevelPass,
			wantDirty: false,
		},
		{
			name:      "docker ps ok whitespace only passes",
			out:       "   \n",
			err:       nil,
			wantLevel: LevelPass,
			wantDirty: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			probes := passingProbes()
			probes.CommandExists = func(name string) bool {
				if name == "docker" {
					return true
				}
				if name == "apt-get" {
					return true
				}
				return false
			}
			probes.CommandOutput = func(ctx context.Context, name string, args ...string) (string, error) {
				if name == "docker" {
					return tc.out, tc.err
				}
				return "", nil
			}
			results, err := RunPreflight(ctx, probes)
			c := findCheck(results, "containers")
			if c == nil {
				t.Fatal("containers check not found")
			}
			if c.Level != tc.wantLevel {
				t.Fatalf("want level %s got %s msg=%q", tc.wantLevel, c.Level, c.Message)
			}
			if c.Dirty != tc.wantDirty {
				t.Fatalf("want dirty=%v got %v msg=%q", tc.wantDirty, c.Dirty, c.Message)
			}
			if tc.wantMsg != "" && !strings.Contains(c.Message, tc.wantMsg) {
				t.Fatalf("expected message to contain %q got %q", tc.wantMsg, c.Message)
			}
			if tc.wantRemed != "" && !strings.Contains(c.Remediation, tc.wantRemed) {
				t.Fatalf("expected remediation to contain %q got %q", tc.wantRemed, c.Remediation)
			}
			// Verify overall RunPreflight error dirty semantics for fail cases
			if tc.wantLevel == LevelFail {
				if err == nil {
					t.Fatalf("expected RunPreflight error on containers fail")
				}
				if pe, ok := err.(*PreflightError); ok {
					if tc.wantDirty && !pe.IsDirty() {
						t.Fatalf("expected dirty error")
					}
					if !tc.wantDirty && pe.IsDirty() {
						t.Fatalf("expected not-dirty error for daemon unreachable")
					}
				}
			} else {
				if err != nil {
					t.Fatalf("expected pass, got error %v", err)
				}
			}
		})
	}
}

