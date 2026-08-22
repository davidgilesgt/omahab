package integrations

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeRunnerHass struct {
	fn func(name string, args ...string) ([]byte, error)
}

func (f fakeRunnerHass) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	return f.fn(name, args...)
}

func TestHassRunnerValidate(t *testing.T) {
	tests := []struct {
		name    string
		server  string
		token   string
		runner  func(string, ...string) ([]byte, error)
		wantErr bool
		errIs   error
	}{
		{
			name:   "success",
			server: "https://ha.example.com",
			token:  "tok123",
			runner: func(n string, args ...string) ([]byte, error) { return []byte("states\n"), nil },
		},
		{
			name:   "missing server",
			server: "",
			token:  "tok",
			runner: func(n string, args ...string) ([]byte, error) { return nil, nil },
			wantErr: true, errIs: ErrHAInvalid,
		},
		{
			name:   "missing token",
			server: "https://ha.example.com",
			token:  "",
			runner: func(n string, args ...string) ([]byte, error) { return nil, nil },
			wantErr: true, errIs: ErrHAInvalid,
		},
		{
			name:   "hass-cli fails",
			server: "https://ha.example.com",
			token:  "bad",
			runner: func(n string, args ...string) ([]byte, error) {
				return []byte("401 Unauthorized"), context.Canceled
			},
			wantErr: true, errIs: ErrHAInvalid,
		},
		{
			name:   "token redacted in error",
			server: "https://ha.example.com",
			token:  "supersecret",
			runner: func(n string, args ...string) ([]byte, error) {
				return []byte("invalid token supersecret"), context.Canceled
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := NewHassRunner(HassRunnerOptions{
				HassCliPath: "hass-cli",
				SkillDir:    t.TempDir(),
				Runner:      fakeRunnerHass{fn: tc.runner},
			})
			err := r.Validate(context.Background(), tc.server, tc.token)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected %v", err)
			}
			if tc.errIs != nil && err != nil && !strings.Contains(err.Error(), tc.errIs.Error()) {
				t.Fatalf("expected err containing %v got %v", tc.errIs, err)
			}
			if tc.name == "token redacted in error" && err != nil && strings.Contains(err.Error(), "supersecret") {
				t.Fatalf("token not redacted: %v", err)
			}
		})
	}
}

func TestHassRunnerInstallSkill(t *testing.T) {
	t.Run("creates skill file", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "skills")
		r := NewHassRunner(HassRunnerOptions{SkillDir: dir, Runner: fakeRunnerHass{fn: func(string, ...string) ([]byte, error) { return nil, nil }}})
		if err := r.InstallSkill(context.Background()); err != nil {
			t.Fatalf("unexpected %v", err)
		}
		path := filepath.Join(dir, "hass-cli.md")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read skill %v", err)
		}
		content := string(data)
		for _, needle := range []string{"hass-cli", "HASS_SERVER", "HASS_TOKEN", "hermes:default"} {
			if !strings.Contains(content, needle) {
				t.Fatalf("skill missing %q", needle)
			}
		}
		if err := r.InstallSkill(context.Background()); err != nil {
			t.Fatalf("second install failed %v", err)
		}
	})
	t.Run("empty dir via direct struct error", func(t *testing.T) {
		r := &RealHassRunner{skillDir: "", runner: fakeRunnerHass{fn: func(string, ...string) ([]byte, error) { return nil, nil }}}
		if err := r.InstallSkill(context.Background()); err == nil {
			t.Fatalf("expected error for empty dir")
		}
	})
	t.Run("mkdir failure", func(t *testing.T) {
		// Use a file as dir to cause MkdirAll failure
		tmp := t.TempDir()
		file := filepath.Join(tmp, "file")
		_ = os.WriteFile(file, []byte("x"), 0o644)
		r := NewHassRunner(HassRunnerOptions{SkillDir: file, Runner: fakeRunnerHass{fn: func(string, ...string) ([]byte, error) { return nil, nil }}})
		if err := r.InstallSkill(context.Background()); err == nil {
			t.Fatalf("expected mkdir error")
		}
	})
}

func TestHassRunnerInstallSkillContextCancel(t *testing.T) {
	dir := t.TempDir()
	r := NewHassRunner(HassRunnerOptions{SkillDir: dir})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := r.InstallSkill(ctx)
	if err == nil {
		t.Fatalf("expected cancel error")
	}
}

func TestNewHassRunnerWithDir(t *testing.T) {
	dir := t.TempDir()
	r := NewHassRunnerWithDir(dir)
	if r == nil {
		t.Fatalf("nil runner")
	}
	rr := r.(*RealHassRunner)
	if rr.skillDir != dir {
		t.Fatalf("want %q got %q", dir, rr.skillDir)
	}
}
