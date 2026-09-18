package providers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubHelper writes an executable shell script emulating the NDJSON protocol.
func stubHelper(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "omahab-provider-auth")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testAuthDirs(t *testing.T) map[string]string {
	t.Helper()
	root := t.TempDir()
	dirs := map[string]string{
		ProviderChatGPT: filepath.Join(root, "chatgpt"),
		ProviderXAI:     filepath.Join(root, "xai"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return dirs
}

// Real device challenge flows to the session; no fake is synthesized.
func TestAdapterChallengeThenComplete(t *testing.T) {
	// Shell builtins only: the adapter runs helpers with PATH=/usr/bin:/bin,
	// which is empty in the Nix sandbox, so external sleep is unavailable.
	// The stub blocks on a sentinel file, so complete can never overtake
	// the connecting assertion below.
	release := filepath.Join(t.TempDir(), "release")
	helper := stubHelper(t, fmt.Sprintf(`echo '{"event":"challenge","provider":"chatgpt","verification_url":"https://auth.openai.com/codex/device","user_code":"AAAA-1111","expires_at":9999999999}'; while [ ! -e %q ]; do :; done; echo '{"event":"complete","provider":"chatgpt"}'`, release))
	a := NewProviderAuthAdapter(ProviderAuthOptions{HelperPath: helper, AuthDirs: testAuthDirs(t)})
	defer a.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := a.Start(ctx, ProviderChatGPT)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if sess.VerificationURL != "https://auth.openai.com/codex/device" {
		t.Fatalf("url = %q", sess.VerificationURL)
	}
	if sess.UserCode == nil || *sess.UserCode != "AAAA-1111" {
		t.Fatalf("code = %v", sess.UserCode)
	}
	if sess.Status != OAuthStatusConnecting {
		t.Fatalf("status = %q", sess.Status)
	}
	if err := os.WriteFile(release, []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Poll converges to connected once the helper completes.
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := a.Poll(context.Background(), sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == OAuthStatusConnected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never connected: %+v", got)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Helper errors map to distinct statuses; tokens never surface.
func TestAdapterErrorMapping(t *testing.T) {
	cases := []struct{ code, want string }{
		{"expired", OAuthStatusExpired},
		{"not_entitled", OAuthStatusNotEntitled},
		{"quota_exceeded", OAuthStatusQuotaExceeded},
		{"state_mismatch", OAuthStatusError},
	}
	for _, tc := range cases {
		helper := stubHelper(t, `echo '{"event":"error","provider":"xai","code":"`+tc.code+`","message":"nope"}'; sleep 30`)
		a := NewProviderAuthAdapter(ProviderAuthOptions{HelperPath: helper, AuthDirs: testAuthDirs(t)})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		sess, err := a.Start(ctx, ProviderXAI)
		cancel()
		a.Close()
		if err != nil {
			t.Fatalf("%s: start: %v", tc.code, err)
		}
		if sess.Status != tc.want {
			t.Fatalf("%s: status = %q, want %q", tc.code, sess.Status, tc.want)
		}
		raw := sess.VerificationURL
		if strings.Contains(raw, "token") {
			t.Fatalf("url leaks: %q", raw)
		}
	}
}

// Cancellation reaps the child; saved credentials remain (no files touched).
func TestAdapterCancelReaps(t *testing.T) {
	helper := stubHelper(t, `echo '{"event":"challenge","provider":"xai","verification_url":"https://x.ai/authorize","callback_port":56121}'; sleep 30`)
	a := NewProviderAuthAdapter(ProviderAuthOptions{HelperPath: helper, AuthDirs: testAuthDirs(t)})
	defer a.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := a.Start(ctx, ProviderXAI)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if sess.CallbackPort == nil || *sess.CallbackPort != 56121 {
		t.Fatalf("port = %v", sess.CallbackPort)
	}
	if err := a.Cancel(context.Background(), sess.ID); err != nil {
		t.Fatal(err)
	}
	got, err := a.Poll(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != OAuthStatusExpired {
		t.Fatalf("cancelled status = %q", got.Status)
	}
}

// Missing helper reports not-configured, never a fake challenge.
func TestAdapterMissingHelperFailsClosed(t *testing.T) {
	a := NewProviderAuthAdapter(ProviderAuthOptions{})
	defer a.Close()
	if _, err := a.Start(context.Background(), ProviderChatGPT); err == nil {
		t.Fatal("expected not-configured")
	}
}

// One subprocess per provider: concurrent starts conflict.
func TestAdapterOneProcPerProvider(t *testing.T) {
	// Builtins only (see above): the stub spins until Close reaps it, so the
	// provider slot stays busy and the conflict below is deterministic.
	helper := stubHelper(t, `echo '{"event":"challenge","provider":"chatgpt","verification_url":"https://auth.openai.com/codex/device","user_code":"X"}'; while :; do :; done`)
	a := NewProviderAuthAdapter(ProviderAuthOptions{HelperPath: helper, AuthDirs: testAuthDirs(t)})
	defer a.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := a.Start(ctx, ProviderChatGPT); err != nil {
		t.Fatalf("first start: %v", err)
	}
	if _, err := a.Start(ctx, ProviderChatGPT); err == nil {
		t.Fatal("expected busy conflict for second start")
	}
}

// Status distinguishes missing files from expired refresh without logging in.
func TestAdapterConnectionStatus(t *testing.T) {
	dirs := testAuthDirs(t)
	helper := stubHelper(t, `echo '{"event":"status","provider":"chatgpt","authenticated":false}'`)
	a := NewProviderAuthAdapter(ProviderAuthOptions{HelperPath: helper, AuthDirs: dirs})
	defer a.Close()
	if got := a.ConnectionStatus(context.Background(), ProviderChatGPT); got != OAuthStatusNotAuthenticated {
		t.Fatalf("no file: %q", got)
	}
	if err := os.WriteFile(filepath.Join(dirs[ProviderChatGPT], "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := a.ConnectionStatus(context.Background(), ProviderChatGPT); got != OAuthStatusExpired {
		t.Fatalf("stale file: %q", got)
	}
}

// Disconnect deletes only the provider's own auth file.
func TestAdapterDisconnectScopesFiles(t *testing.T) {
	dirs := testAuthDirs(t)
	chatFile := filepath.Join(dirs[ProviderChatGPT], "auth.json")
	xaiFile := filepath.Join(dirs[ProviderXAI], "auth.json")
	for _, f := range []string{chatFile, xaiFile} {
		if err := os.WriteFile(f, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	helper := stubHelper(t, `exit 0`)
	a := NewProviderAuthAdapter(ProviderAuthOptions{HelperPath: helper, AuthDirs: dirs})
	defer a.Close()
	if err := a.Disconnect(context.Background(), ProviderChatGPT); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(chatFile); !os.IsNotExist(err) {
		t.Fatal("chatgpt auth must be deleted")
	}
	if _, err := os.Stat(xaiFile); err != nil {
		t.Fatal("xai auth must be untouched")
	}
}
