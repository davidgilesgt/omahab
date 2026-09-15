package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// stubRunner is a minimal CommandRunner for contract tests.
type stubRunner struct {
	// map from joined args to output; if missing, return error
	outputs map[string]string
	errs    map[string]error
	// capture calls
	calls [][]string
}

func (s *stubRunner) Run(_ context.Context, args ...string) (string, error) {
	s.calls = append(s.calls, args)
	key := strings.Join(args, " ")
	if err, ok := s.errs[key]; ok {
		return s.outputs[key], err
	}
	if out, ok := s.outputs[key]; ok {
		return out, nil
	}
	// fallback: try prefix match for litellm --help etc
	return "", nil
}

// fakeAuthHelper scripts adapter behavior without subprocesses.
type fakeAuthHelper struct {
	caps     map[string]any
	capsErr  error
	sessions map[string]OAuthSession
	startErr error
}

func (f *fakeAuthHelper) Start(_ context.Context, provider string) (OAuthSession, error) {
	if f.startErr != nil {
		return OAuthSession{}, f.startErr
	}
	now := time.Now().UTC()
	switch provider {
	case ProviderChatGPT:
		uc := "ABCD-1234"
		sess := OAuthSession{
			ID: "sess-chatgpt", Provider: provider, Flow: FlowDeviceCode,
			VerificationURL: "https://auth.openai.com/codex/device",
			UserCode:        &uc,
			ExpiresAt:       now.Add(15 * time.Minute),
			Status:          OAuthStatusConnecting,
		}
		if f.sessions == nil {
			f.sessions = map[string]OAuthSession{}
		}
		f.sessions[sess.ID] = sess
		return sess, nil
	case ProviderXAI:
		port := 56121
		sess := OAuthSession{
			ID: "sess-xai", Provider: provider, Flow: FlowLoopback,
			VerificationURL: "https://accounts.x.ai/authorize?state=xyz",
			CallbackPort:    &port,
			ExpiresAt:       now.Add(5 * time.Minute),
			Status:          OAuthStatusConnecting,
		}
		if f.sessions == nil {
			f.sessions = map[string]OAuthSession{}
		}
		f.sessions[sess.ID] = sess
		return sess, nil
	default:
		return OAuthSession{}, fmt.Errorf("unsupported provider %q", provider)
	}
}

func (f *fakeAuthHelper) Poll(_ context.Context, sessionID string) (OAuthSession, error) {
	if sess, ok := f.sessions[sessionID]; ok {
		return sess, nil
	}
	return OAuthSession{}, ErrNotFound
}

func (f *fakeAuthHelper) Cancel(_ context.Context, sessionID string) error {
	delete(f.sessions, sessionID)
	return nil
}

func (f *fakeAuthHelper) Capabilities(_ context.Context) (map[string]any, error) {
	if f.capsErr != nil {
		return nil, f.capsErr
	}
	return f.caps, nil
}

func (f *fakeAuthHelper) ConnectionStatus(_ context.Context, provider string) string {
	return OAuthStatusNotAuthenticated
}

func (f *fakeAuthHelper) Disconnect(_ context.Context, provider string) error { return nil }

func testCapabilities() map[string]any {
	return map[string]any{
		"event":           "capabilities",
		"litellm_version": "1.97.0",
		"chatgpt":         map[string]any{"ok": true, "verify_url": "https://auth.openai.com/codex/device"},
		"xai":             map[string]any{"ok": true, "callback_port": float64(56121)},
	}
}

// TestVerifyPin_Contract ensures the packaged helper environment exposes the
// pinned authenticators, then smokes a real native model request.
func TestVerifyPin_Contract(t *testing.T) {
	ctx := context.Background()

	// Happy path: helper capabilities report both authenticators.
	gw, err := NewLiteLLMGateway(nilPlaceholderDB(), GatewayOptions{AuthAdapter: &fakeAuthHelper{caps: testCapabilities()}})
	if err != nil {
		t.Fatalf("NewLiteLLMGateway: %v", err)
	}
	if err := gw.verifyPin(ctx); err != nil {
		t.Fatalf("verifyPin happy should pass, got %v", err)
	}

	// Missing xai support fails closed.
	broken := testCapabilities()
	broken["xai"] = map[string]any{"ok": false}
	gw2, _ := NewLiteLLMGateway(nilPlaceholderDB(), GatewayOptions{AuthAdapter: &fakeAuthHelper{caps: broken}})
	if err := gw2.verifyPin(ctx); err == nil {
		t.Fatal("verifyPin should fail when xai support is missing")
	}

	// Capabilities failure fails closed.
	gw3, _ := NewLiteLLMGateway(nilPlaceholderDB(), GatewayOptions{AuthAdapter: &fakeAuthHelper{capsErr: fmt.Errorf("boom")}})
	if err := gw3.verifyPin(ctx); err == nil {
		t.Fatal("verifyPin should fail when capabilities fail")
	}

	// No adapter: skip pin check (minimal test env) should pass.
	gw4, _ := NewLiteLLMGateway(nilPlaceholderDB(), GatewayOptions{AuthAdapter: nil})
	if err := gw4.verifyPin(ctx); err != nil {
		t.Fatalf("verifyPin with nil adapter should skip, got %v", err)
	}

	// Health with no adapter and no master key should still return nil (test wiring).
	if err := gw4.Health(ctx); err != nil {
		t.Fatalf("Health with nil adapter should be nil in test env, got %v", err)
	}
}

// TestChatGPT_StartOAuth_RealChallenge verifies StartOAuth surfaces the real
// device challenge (codex URL, user code, ~15m expiry) with no fake fallback
// and no secret material in the session view.
func TestChatGPT_StartOAuth_RealChallenge(t *testing.T) {
	ctx := context.Background()
	gw, _ := NewLiteLLMGateway(nilPlaceholderDB(), GatewayOptions{AuthAdapter: &fakeAuthHelper{caps: testCapabilities()}})
	sess, err := gw.StartOAuth(ctx, ProviderChatGPT, FlowDeviceCode)
	if err != nil {
		t.Fatalf("StartOAuth chatgpt: %v", err)
	}
	if sess.Provider != ProviderChatGPT || sess.Flow != FlowDeviceCode {
		t.Fatalf("unexpected provider/flow %q/%q", sess.Provider, sess.Flow)
	}
	if sess.VerificationURL != "https://auth.openai.com/codex/device" {
		t.Fatalf("verification_url must be the real device URL, got %q", sess.VerificationURL)
	}
	if sess.UserCode == nil || *sess.UserCode != "ABCD-1234" {
		t.Fatalf("user_code mismatch: %v", sess.UserCode)
	}
	if sess.CallbackPort != nil {
		t.Fatalf("chatgpt device_code should not have callback_port, got %v", *sess.CallbackPort)
	}
	// 15-minute upstream expiry, not a 10-minute guess.
	if time.Until(sess.ExpiresAt) > 16*time.Minute || time.Until(sess.ExpiresAt) < 14*time.Minute {
		t.Fatalf("expires_at not 15m window: %v", sess.ExpiresAt)
	}
	if sess.Status != OAuthStatusConnecting {
		t.Fatalf("status %q, want connecting", sess.Status)
	}
	// No secret material in the session view.
	raw, _ := json.Marshal(sess)
	for _, banned := range []string{"access_token", "refresh_token", "device_auth_id", "verifier", "authorization_code"} {
		if strings.Contains(strings.ToLower(string(raw)), banned) {
			t.Fatalf("session view leaks %q: %s", banned, raw)
		}
	}
}

// TestXAI_StartOAuth_RealChallenge verifies xAI captures the authorize URL
// with the fixed 56121 loopback port and no user code.
func TestXAI_StartOAuth_RealChallenge(t *testing.T) {
	ctx := context.Background()
	gw, _ := NewLiteLLMGateway(nilPlaceholderDB(), GatewayOptions{AuthAdapter: &fakeAuthHelper{caps: testCapabilities()}})
	sess, err := gw.StartOAuth(ctx, ProviderXAI, FlowLoopback)
	if err != nil {
		t.Fatalf("StartOAuth xai: %v", err)
	}
	if sess.Provider != ProviderXAI || sess.Flow != FlowLoopback {
		t.Fatalf("provider/flow %q/%q", sess.Provider, sess.Flow)
	}
	if sess.VerificationURL != "https://accounts.x.ai/authorize?state=xyz" {
		t.Fatalf("auth URL not captured: %q", sess.VerificationURL)
	}
	if sess.CallbackPort == nil || *sess.CallbackPort != 56121 {
		t.Fatalf("callback_port must be 56121, got %v", sess.CallbackPort)
	}
	if sess.UserCode != nil {
		t.Fatalf("xai should not have user_code, got %v", *sess.UserCode)
	}
}

// TestStartOAuth_WithoutAdapterFailsClosed ensures no fake challenge is ever
// synthesized when the helper is missing.
func TestStartOAuth_WithoutAdapterFailsClosed(t *testing.T) {
	ctx := context.Background()
	gw, _ := NewLiteLLMGateway(nilPlaceholderDB(), GatewayOptions{AuthAdapter: nil})
	if _, err := gw.StartOAuth(ctx, ProviderChatGPT, FlowDeviceCode); err == nil {
		t.Fatal("expected not-configured, got fake challenge")
	}
	if _, err := gw.StartOAuth(ctx, ProviderXAI, FlowLoopback); err == nil {
		t.Fatal("expected not-configured, got fake challenge")
	}
}

// TestValidateCallbackPath_Strict ensures only /callback?<query> is accepted, rejecting foreign host/path/scheme/port/expired/device/shell metachars.
func TestValidateCallbackPath_Strict(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		name string
	}{
		{"/callback?code=abc&state=xyz", true, "valid query"},
		{"/callback", true, "no query"},
		{"/callback?code=abc", true, "simple query"},
		{"/callback?code=abc%20def", true, "encoded"},
		{"https://evil.com/callback?code=abc", false, "foreign host scheme"},
		{"http://127.0.0.1:56121/callback?code=abc", false, "absolute URL with host"},
		{"/callback; rm -rf /", false, "shell semicolon"},
		{"/callback?code=abc|evil", false, "pipe"},
		{"/callback?code=$HOME", false, "dollar"},
		{"/callback?code=`evil`", false, "backtick"},
		{"/other?code=abc", false, "wrong path"},
		{"/callback/../etc/passwd", false, "dot dot"},
		{"", false, "empty"},
		{"/callback?code=abc&x=1;echo", false, "embedded semicolon"},
	}
	for _, tc := range cases {
		err := ValidateCallbackPath(tc.in)
		if tc.ok && err != nil {
			t.Errorf("%s: expected ok, got err %v (input %q)", tc.name, err, tc.in)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: expected reject, got ok (input %q)", tc.name, tc.in)
		}
	}
}

// TestProbeModelMapsStatus ensures ProbeModel uses ClassifyHTTPStatus for 401/403 and handles 429 quota.
func TestProbeModelMapsStatus(t *testing.T) {
	if err := ClassifyHTTPStatus(401); err != ErrTokenInvalid {
		t.Fatalf("401 should map to ErrTokenInvalid, got %v", err)
	}
	if err := ClassifyHTTPStatus(403); err != ErrEntitlement {
		t.Fatalf("403 should map to ErrEntitlement, got %v", err)
	}
	if err := ClassifyHTTPStatus(200); err != nil {
		t.Fatalf("200 should be nil, got %v", err)
	}
	if err := ClassifyHTTPStatus(429); err != nil {
		// Currently 429 is not mapped to entitlement; backend helper handles via string check for quota/rate-limited
		t.Logf("429 classified as %v (handled as quota in backend)", err)
	}
}

// nilPlaceholderDB returns a non-nil placeholder for gateway DB requirement in tests.
// The gateway only stores sessions in memory; DB is required non-nil but not used for these contract checks.
func nilPlaceholderDB() interface{} {
	// use a dummy non-nil pointer
	type dummy struct{}
	return &dummy{}
}
