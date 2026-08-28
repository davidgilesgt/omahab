package edge

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/exposure"
)

func TestRenderConfig_ListenAndCloudflareIssuer(t *testing.T) {
	token := "test-token-abc123"
	domain := "example.com"
	routes := []exposure.Route{{Hostname: "id.example.com", Upstream: "http://pocket-id:8080"}}
	b, err := RenderConfig(domain, token, routes)
	if err != nil {
		t.Fatalf("RenderConfig failed: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Check listen includes :443 and :80
	apps, ok := raw["apps"].(map[string]any)
	if !ok {
		t.Fatalf("missing apps")
	}
	httpCfg, ok := apps["http"].(map[string]any)
	if !ok {
		t.Fatalf("missing http")
	}
	servers, ok := httpCfg["servers"].(map[string]any)
	if !ok {
		t.Fatalf("missing servers")
	}
	mainSrv, ok := servers["main"].(map[string]any)
	if !ok {
		t.Fatalf("missing main")
	}
	listen, ok := mainSrv["listen"].([]any)
	if !ok {
		t.Fatalf("missing listen: %T", mainSrv["listen"])
	}
	found443, found80 := false, false
	for _, v := range listen {
		if s, ok := v.(string); ok {
			if s == ":443" {
				found443 = true
			}
			if s == ":80" {
				found80 = true
			}
		}
	}
	if !found443 || !found80 {
		t.Fatalf("listen must include :443 and :80, got %v", listen)
	}
	// Check cloudflare DNS-01 issuer
	tlsCfg, ok := apps["tls"].(map[string]any)
	if !ok {
		t.Fatalf("missing tls")
	}
	automation, ok := tlsCfg["automation"].(map[string]any)
	if !ok {
		t.Fatalf("missing automation")
	}
	policies, ok := automation["policies"].([]any)
	if !ok || len(policies) == 0 {
		t.Fatalf("missing policies")
	}
	pol, ok := policies[0].(map[string]any)
	if !ok {
		t.Fatalf("policy not map")
	}
	issuers, ok := pol["issuers"].([]any)
	if !ok || len(issuers) == 0 {
		t.Fatalf("missing issuers")
	}
	issuer, ok := issuers[0].(map[string]any)
	if !ok {
		t.Fatalf("issuer not map")
	}
	if issuer["module"] != "acme" {
		t.Fatalf("issuer module != acme: %v", issuer["module"])
	}
	challenges, ok := issuer["challenges"].(map[string]any)
	if !ok {
		t.Fatalf("missing challenges")
	}
	dns, ok := challenges["dns"].(map[string]any)
	if !ok {
		t.Fatalf("missing dns challenge")
	}
	provider, ok := dns["provider"].(map[string]any)
	if !ok {
		t.Fatalf("missing provider")
	}
	if provider["name"] != "cloudflare" {
		t.Fatalf("provider name != cloudflare: %v", provider["name"])
	}
	if provider["api_token"] != token {
		t.Fatalf("api_token mismatch: got %v want %v", provider["api_token"], token)
	}
	// Ensure listen appears in JSON string as specified
	js := string(b)
	if !strings.Contains(js, `":443"`) || !strings.Contains(js, `":80"`) {
		t.Fatalf("JSON missing listen strings: %s", js)
	}
}

func TestRenderConfig_RejectsEmptyToken(t *testing.T) {
	_, err := RenderConfig("example.com", "", nil)
	if err == nil {
		t.Fatalf("expected error for empty token")
	}
	if !strings.Contains(err.Error(), "dns token is required") {
		t.Fatalf("unexpected error: %v", err)
	}
	_, err = RenderConfig("example.com", "   ", nil)
	if err == nil {
		t.Fatalf("expected error for whitespace token")
	}
}
