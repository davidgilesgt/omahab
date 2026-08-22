package installer

import (
	"os"
	"runtime"
	"testing"
)

// TestLiveActiveSSHSessionEnvFastPath pins the unprivileged path: an inherited
// SSH_CONNECTION is sufficient, and the client address is extracted from it.
func TestLiveActiveSSHSessionEnvFastPath(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "203.0.113.9 51422 192.0.2.1 22")
	ok, addr, err := liveActiveSSHSession()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected session to be detected via SSH_CONNECTION")
	}
	if addr != "203.0.113.9" {
		t.Fatalf("expected client address 203.0.113.9, got %q", addr)
	}
}

// TestLiveActiveSSHSessionSurvivesEnvReset is the regression test for the
// sudo case: sudo's env_reset strips SSH_* variables, so detection must not
// depend on them when the process genuinely descends from a live sshd.
func TestLiveActiveSSHSessionSurvivesEnvReset(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "")
	if !sshdAncestor() {
		t.Skip("no live sshd ancestor in this environment (expected in CI)")
	}
	ok, _, err := liveActiveSSHSession()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("sshd ancestor present but session not detected — installer run via sudo over SSH would abort")
	}
}

// TestProcNameAndParentSelf checks /proc parsing on Linux; on other systems
// the helper must fail cleanly rather than report bogus data.
func TestProcNameAndParentSelf(t *testing.T) {
	name, ppid, err := procNameAndParent(os.Getpid())
	if runtime.GOOS != "linux" {
		if err == nil {
			t.Fatalf("expected error on %s", runtime.GOOS)
		}
		return
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name == "" || ppid < 1 {
		t.Fatalf("bad values: name=%q ppid=%d", name, ppid)
	}
	if _, _, err := procNameAndParent(-1); err == nil {
		t.Fatal("expected error for nonexistent pid")
	}
}
