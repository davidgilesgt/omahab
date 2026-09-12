package controlplane

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gateWithCode returns a gate holding an active code without touching the
// filesystem (/run/omahab is not writable in tests).
func gateWithCode(code string) *BootstrapGate {
	g := NewBootstrapGate()
	sum := sha256.Sum256([]byte(code))
	g.codeHash = sum[:]
	return g
}

func TestClaimLANExceptionConsumes(t *testing.T) {
	t.Setenv("OMAHAB_PLACEMENT", "")
	g := gateWithCode("secretcode1")
	if err := g.Claim("", "192.168.1.5"); err != nil {
		t.Fatalf("lan empty claim: %v", err)
	}
	// First-claimer wins: the code is consumed.
	if err := g.Claim("secretcode1", "192.168.1.5"); err == nil {
		t.Fatal("consumed code must not claim again")
	}
}

func TestClaimLANExceptionLoopback(t *testing.T) {
	t.Setenv("OMAHAB_PLACEMENT", "lan")
	g := gateWithCode("secretcode1")
	if err := g.Claim("  ", "127.0.0.1"); err != nil {
		t.Fatalf("loopback empty claim: %v", err)
	}
}

func TestClaimEmptyPublicSourceRejected(t *testing.T) {
	t.Setenv("OMAHAB_PLACEMENT", "")
	g := gateWithCode("secretcode1")
	if err := g.Claim("", "8.8.8.8"); err == nil {
		t.Fatal("empty code from public source must fail")
	} else if !strings.Contains(err.Error(), "invalid code") {
		t.Fatalf("want invalid code, got %v", err)
	}
	// Code must not be consumed by the failed attempt.
	if err := g.Claim("secretcode1", "8.8.8.8"); err != nil {
		t.Fatalf("real code must still work: %v", err)
	}
}

func TestClaimEmptyVPSRejected(t *testing.T) {
	t.Setenv("OMAHAB_PLACEMENT", "vps")
	g := gateWithCode("secretcode1")
	if err := g.Claim("", "192.168.1.5"); err == nil {
		t.Fatal("empty code on vps placement must fail")
	}
	if err := g.Claim("secretcode1", "192.168.1.5"); err != nil {
		t.Fatalf("real code must still work on vps: %v", err)
	}
}

func TestClaimNormalPathUnchanged(t *testing.T) {
	t.Setenv("OMAHAB_PLACEMENT", "lan")
	g := gateWithCode("secretcode1")
	if err := g.Claim("wrongcode00", "192.168.1.5"); err == nil {
		t.Fatal("wrong code must fail")
	}
	if err := g.Claim("secretcode1", "192.168.1.5"); err != nil {
		t.Fatalf("correct code: %v", err)
	}
	if err := g.Claim("secretcode1", "192.168.1.5"); err == nil {
		t.Fatal("code must be single-use")
	}
}

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
