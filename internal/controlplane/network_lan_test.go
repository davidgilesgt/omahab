package controlplane

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPlacementDefault(t *testing.T) {
	t.Setenv("OMAHAB_PLACEMENT", "")
	if Placement() != "lan" {
		t.Fatal("unset placement must default to lan")
	}
	t.Setenv("OMAHAB_PLACEMENT", "vps")
	if Placement() != "vps" {
		t.Fatal("vps placement must be honored")
	}
	t.Setenv("OMAHAB_PLACEMENT", "something-else")
	if Placement() != "lan" {
		t.Fatal("unknown placement must fall back to lan")
	}
}

const sampleNftList = "table inet omahab {\n" +
	"\tchain input {\n" +
	"\t\ttype filter hook input priority 10; policy drop;\n" +
	"\t\tiifname \"lo\" accept\n" +
	"\t\ttcp dport 22 accept comment \"ssh\" handle 3\n" +
	"\t\tiifname \"tailscale0\" tcp dport 8484 accept comment \"omahab dashboard via tailscale\" handle 7\n" +
	"\t\ttcp dport 8484 ip saddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 } accept comment \"omahab dashboard LAN\" handle 11\n" +
	"\t\tip6 saddr fc00::/7 tcp dport 8484 accept comment \"omahab dashboard ULA\" handle 12\n" +
	"\t\tip6 saddr fe80::/10 tcp dport 8484 accept comment \"omahab dashboard link-local\" handle 13\n" +
	"\t}\n" +
	"}\n"

func TestLanHandles(t *testing.T) {
	got := lanHandles(sampleNftList)
	if len(got) != 3 || got[0] != "11" || got[1] != "12" || got[2] != "13" {
		t.Fatalf("handles = %v, want [11 12 13]", got)
	}
	if len(lanHandles("tcp dport 22 accept comment \"ssh\" handle 3\n")) != 0 {
		t.Fatal("ssh rule must never match")
	}
}

// fakeNft records nft invocations; listOut/listErr control the list call.
type fakeNft struct {
	calls   [][]string
	listOut string
	listErr error
	delErr  error
}

func (f *fakeNft) run(args ...string) (string, error) {
	f.calls = append(f.calls, args)
	if len(args) > 0 && args[0] == "--handle" {
		return f.listOut, f.listErr
	}
	if len(args) > 0 && args[0] == "delete" {
		return "", f.delErr
	}
	return "", nil
}

func (f *fakeNft) deletes() []string {
	var out []string
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == "delete" {
			out = append(out, c[len(c)-1])
		}
	}
	return out
}

func withLanTestSeams(t *testing.T, f *fakeNft, restartErr error) (*Backend, func() int) {
	t.Helper()
	oldPath, oldExec, oldRestart := lanClosedPath, nftExec, nftablesRestart
	lanClosedPath = filepath.Join(t.TempDir(), "lan-closed")
	nftExec = f.run
	restarts := 0
	nftablesRestart = func() error {
		restarts++
		return restartErr
	}
	t.Cleanup(func() {
		lanClosedPath, nftExec, nftablesRestart = oldPath, oldExec, oldRestart
	})
	return &Backend{}, func() int { return restarts }
}

func TestCloseLANDeletesRulesAndWritesSentinel(t *testing.T) {
	f := &fakeNft{listOut: sampleNftList}
	b, _ := withLanTestSeams(t, f, nil)
	if err := b.CloseLAN(); err != nil {
		t.Fatalf("CloseLAN: %v", err)
	}
	if got := f.deletes(); len(got) != 3 || got[0] != "11" || got[1] != "12" || got[2] != "13" {
		t.Fatalf("deletes = %v, want [11 12 13]", got)
	}
	if !LANClosed() {
		t.Fatal("sentinel must be present after close")
	}
}

func TestCloseLANIdempotentWithoutRules(t *testing.T) {
	f := &fakeNft{listOut: "table inet omahab {\n\tchain input {\n\t\ttcp dport 22 accept comment \"ssh\" handle 3\n\t}\n}\n"}
	b, _ := withLanTestSeams(t, f, nil)
	if err := b.CloseLAN(); err != nil {
		t.Fatalf("CloseLAN: %v", err)
	}
	if len(f.deletes()) != 0 {
		t.Fatalf("no deletes expected, got %v", f.deletes())
	}
	if !LANClosed() {
		t.Fatal("sentinel must be written even when no rules matched")
	}
}

func TestCloseLANListErrorNoSentinel(t *testing.T) {
	f := &fakeNft{listErr: os.ErrPermission}
	b, _ := withLanTestSeams(t, f, nil)
	if err := b.CloseLAN(); err == nil {
		t.Fatal("list failure must propagate")
	}
	if LANClosed() {
		t.Fatal("sentinel must not be written on failure")
	}
}

func TestOpenLANRestoresTable(t *testing.T) {
	f := &fakeNft{listOut: sampleNftList}
	b, restarts := withLanTestSeams(t, f, nil)
	if err := b.CloseLAN(); err != nil {
		t.Fatalf("CloseLAN: %v", err)
	}
	if err := b.OpenLAN(); err != nil {
		t.Fatalf("OpenLAN: %v", err)
	}
	if LANClosed() {
		t.Fatal("sentinel must be cleared after open")
	}
	if restarts() != 1 {
		t.Fatalf("nftables restart called %d times, want 1", restarts())
	}
}

func TestReapplyLANClosedIfNeeded(t *testing.T) {
	f := &fakeNft{listOut: sampleNftList}
	b, _ := withLanTestSeams(t, f, nil)
	// No sentinel: no nft traffic at all.
	if err := b.ReapplyLANClosedIfNeeded(); err != nil {
		t.Fatalf("reapply without sentinel: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("no nft calls expected, got %v", f.calls)
	}
	// With sentinel: deletion re-applied.
	if err := os.WriteFile(lanClosedPath, []byte("closed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := b.ReapplyLANClosedIfNeeded(); err != nil {
		t.Fatalf("reapply with sentinel: %v", err)
	}
	if got := f.deletes(); len(got) != 3 {
		t.Fatalf("deletes = %v, want 3 handles", got)
	}
}
