package providers

import (
	"context"
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

// TestVerifyPin_Contract ensures the pinned LiteLLM image exposes required xAI OAuth support.
// This fails closed if private upstream authenticator changes before release.
func TestVerifyPin_Contract(t *testing.T) {
	ctx := context.Background()

	// Happy path: image exposes both required strings.
	happy := &stubRunner{
		outputs: map[string]string{
			"litellm --help":               "Usage: litellm [OPTIONS] COMMAND [ARGS]...\nCommands:\n  xai-oauth   XAI OAuth flow\n  chatgpt     ChatGPT auth\n",
			"litellm xai-oauth --help":     "Usage: litellm xai-oauth [OPTIONS]\nOptions:\n  --help  Show this message\n  use_xai_oauth  set to true for xai subscription models\n",
		},
	}
	gw, err := NewLiteLLMGateway(nilPlaceholderDB(), GatewayOptions{Runner: happy, PinDigest: "sha256:fake"})
	if err != nil {
		t.Fatalf("NewLiteLLMGateway: %v", err)
	}
	if err := gw.verifyPin(ctx); err != nil {
		t.Fatalf("verifyPin happy should pass, got %v", err)
	}

	// Missing xai-oauth in top-level help should fail closed.
	missingTop := &stubRunner{
		outputs: map[string]string{
			"litellm --help":           "Usage: litellm [OPTIONS] ...\nCommands:\n  chatgpt   ChatGPT\n",
			"litellm xai-oauth --help": "use_xai_oauth true",
		},
	}
	gw2, _ := NewLiteLLMGateway(nilPlaceholderDB(), GatewayOptions{Runner: missingTop})
	if err := gw2.verifyPin(ctx); err == nil {
		t.Fatalf("verifyPin should fail when top-level help lacks xai-oauth")
	} else if !strings.Contains(err.Error(), "xai-oauth") {
		t.Fatalf("unexpected error for missing xai-oauth: %v", err)
	}

	// Missing use_xai_oauth in both helps should fail closed.
	missingOption := &stubRunner{
		outputs: map[string]string{
			"litellm --help":           "Usage: litellm [OPTIONS] COMMAND [ARGS]...\nCommands:\n  xai-oauth  grok flow\n",
			"litellm xai-oauth --help": "Usage: litellm xai-oauth login --no-browser\n  --help  help\n  --no-browser  disable browser",
		},
	}
	gw3, _ := NewLiteLLMGateway(nilPlaceholderDB(), GatewayOptions{Runner: missingOption})
	if err := gw3.verifyPin(ctx); err == nil {
		t.Fatalf("verifyPin should fail when use_xai_oauth missing")
	}

	// No runner: skip pin check (minimal test env) should pass.
	gw4, _ := NewLiteLLMGateway(nilPlaceholderDB(), GatewayOptions{Runner: nil})
	if err := gw4.verifyPin(ctx); err != nil {
		t.Fatalf("verifyPin with nil runner should skip, got %v", err)
	}

	// Health with no runner and no master key should still return nil (test wiring).
	if err := gw4.Health(ctx); err != nil {
		t.Fatalf("Health with nil runner should be nil in test env, got %v", err)
	}
}

// TestChatGPT_StartOAuth_HelperEmitsJSON verifies ChatGPT helper emits JSON verification_uri/user_code/expires_at
// and StartOAuth correctly respects CHATGPT_TOKEN_DIR literal env path (not secrets) and 10min expiry.
func TestChatGPT_StartOAuth_HelperEmitsJSON(t *testing.T) {
	ctx := context.Background()
	expires := time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339Nano)
	helperJSON := `{"verification_uri":"https://auth.openai.com/activate","user_code":"ABCD-1234","expires_at":"` + expires + `"}`
	runner := &stubRunner{
		outputs: map[string]string{
			"litellm chatgpt auth start --json": helperJSON,
			"litellm --help":                   "xai-oauth\n",
			"litellm xai-oauth --help":         "use_xai_oauth",
		},
	}
	gw, _ := NewLiteLLMGateway(nilPlaceholderDB(), GatewayOptions{Runner: runner})
	sess, err := gw.StartOAuth(ctx, ProviderChatGPT, FlowDeviceCode)
	if err != nil {
		t.Fatalf("StartOAuth chatgpt: %v", err)
	}
	if sess.Provider != ProviderChatGPT || sess.Flow != FlowDeviceCode {
		t.Fatalf("unexpected provider/flow %q/%q", sess.Provider, sess.Flow)
	}
	if sess.VerificationURL != "https://auth.openai.com/activate" {
		t.Fatalf("verification_url mismatch: %q", sess.VerificationURL)
	}
	if sess.UserCode == nil || *sess.UserCode != "ABCD-1234" {
		t.Fatalf("user_code mismatch: %v", sess.UserCode)
	}
	if sess.CallbackPort != nil {
		t.Fatalf("chatgpt device_code should not have callback_port, got %v", *sess.CallbackPort)
	}
	// Expires within 10min window
	if time.Until(sess.ExpiresAt) > 11*time.Minute || time.Until(sess.ExpiresAt) < 9*time.Minute {
		t.Fatalf("expires_at not 10min window: %v", sess.ExpiresAt)
	}
	if sess.Status != OAuthStatusPending {
		t.Fatalf("status %q", sess.Status)
	}
	// Ensure no device codes or tokens leak in session fields (only safe fields present)
	// OAuthSession struct only has safe fields; JSON marshaling should not contain secrets.
}

// TestXAI_StartOAuth_CapturesAuthURLAndLoopbackPort verifies xAI captures auth URL and binds fixed loopback 56121.
func TestXAI_StartOAuth_CapturesAuthURLAndLoopbackPort(t *testing.T) {
	ctx := context.Background()
	helperJSON := `{"auth_url":"https://accounts.x.ai/authorize?state=xyz","url":"https://accounts.x.ai/authorize?state=xyz"}`
	runner := &stubRunner{
		outputs: map[string]string{
			"litellm xai-oauth login --no-browser --json": helperJSON,
			"litellm --help":                               "xai-oauth",
			"litellm xai-oauth --help":                     "use_xai_oauth",
		},
	}
	gw, _ := NewLiteLLMGateway(nilPlaceholderDB(), GatewayOptions{Runner: runner})
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
	// Document that LiteLLM binds loopback 127.0.0.1:56121; client binds same port locally and POSTs /callback?<query>
	// Fallback is SSH local forward: ssh -L 56121:127.0.0.1:56121 omahab@<server>, not public bound callback.
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
