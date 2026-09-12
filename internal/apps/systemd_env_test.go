package apps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/domain"
)

// Reconcile Deploys must not drop secret-derived keys (e.g. pocket-id
// ENCRYPTION_KEY) written by renderNativeAppEnv and absent from spec.Env.
func TestWriteEnvFileMergesOverExisting(t *testing.T) {
	dir := t.TempDir()
	r := NewSystemdRunner(nil, dir, nil)
	app := domain.Application{ID: "test", BundleID: "pocket-id"}
	if err := os.WriteFile(filepath.Join(dir, "pocket-id.env"), []byte("ENCRYPTION_KEY=server-secret\nOLD=A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := DeploySpec{Env: []string{"OLD=B", "NEW=C"}}
	if err := r.writeEnvFile(app, spec); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "pocket-id.env"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		name, value, _ := strings.Cut(line, "=")
		got[name] = value
	}
	if got["ENCRYPTION_KEY"] != "server-secret" {
		t.Fatalf("preserved key lost: %q", string(raw))
	}
	if got["OLD"] != "B" || got["NEW"] != "C" {
		t.Fatalf("spec overlay wrong: %q", string(raw))
	}
}
