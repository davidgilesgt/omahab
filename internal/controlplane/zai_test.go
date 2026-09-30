package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/providers"
)

// Unauthorized Coding Plan saves fail with no silent paid fallback; the
// backend never surfaces an error-free connection in that state.
func TestConnectZAI_CodingRequiresAck(t *testing.T) {
	gw := newFakeNativeGateway()
	b := newModelSetupBackend(t, gw)
	b.providers.SetVirtualKeyGateway(gw)
	_, err := b.ConnectZAI(context.Background(), apitypes.ConnectZAIRequest{
		Plan: "coding", APIKey: "sk-coding",
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "authoriz") {
		t.Fatalf("coding without ack = %v, want authorization error", err)
	}
}

// PAYG connects through the PAYG endpoint only; the coding endpoint is never
// touched and no deployment aliases are disturbed.
func TestConnectZAI_PAYGUsesPAYGEndpoint(t *testing.T) {
	gw := newFakeNativeGateway()
	b := newModelSetupBackend(t, gw)
	b.providers.SetVirtualKeyGateway(gw)
	conn, err := b.ConnectZAI(context.Background(), apitypes.ConnectZAIRequest{
		Plan: "payg", APIKey: "sk-payg", Model: "glm-4.6",
	})
	if err != nil {
		t.Fatalf("connect payg: %v", err)
	}
	if conn.APIBase != providers.ZAIPAYGBase {
		t.Fatalf("api_base = %q, want %q", conn.APIBase, providers.ZAIPAYGBase)
	}
	if conn.Plan != providers.ZAIPlanPAYG || conn.Model != "openai/glm-4.6" || conn.Provider != providers.ProviderZAI {
		t.Fatalf("connection = %+v", conn)
	}
	d, ok := gw.getByName(conn.Deployment)
	if !ok {
		t.Fatalf("deployment %q missing from gateway", conn.Deployment)
	}
	params := d.LitellmParams
	if params["api_base"] != providers.ZAIPAYGBase {
		t.Fatalf("gateway api_base = %v, want PAYG", params["api_base"])
	}
	if base, _ := params["api_base"].(string); strings.Contains(strings.TrimSpace(strings.ToLower(base)), "coding") {
		t.Fatalf("PAYG deployment leaks coding endpoint: %v", params)
	}
}

// Authorized Coding Plan use routes to the coding endpoint with the
// acknowledgement recorded; unauthorized terms are never implied.
func TestConnectZAI_CodingUsesCodingEndpoint(t *testing.T) {
	gw := newFakeNativeGateway()
	b := newModelSetupBackend(t, gw)
	b.providers.SetVirtualKeyGateway(gw)
	conn, err := b.ConnectZAI(context.Background(), apitypes.ConnectZAIRequest{
		Plan: "coding", APIKey: "sk-coding", AuthorizedCodingUse: true, Model: "glm-4.6",
	})
	if err != nil {
		t.Fatalf("connect coding: %v", err)
	}
	if conn.APIBase != providers.ZAICodingBase {
		t.Fatalf("api_base = %q, want %q", conn.APIBase, providers.ZAICodingBase)
	}
	d, ok := gw.getByName(conn.Deployment)
	if !ok {
		t.Fatalf("deployment %q missing from gateway", conn.Deployment)
	}
	if d.LitellmParams["api_base"] != providers.ZAICodingBase {
		t.Fatalf("gateway api_base = %v, want coding", d.LitellmParams)
	}
}

// Disconnect removes only the z.ai connection's credential and secret;
// aliases still referencing the deployment block instead of silently
// breaking routing.
func TestDisconnectZAI_KeepsUnrelated(t *testing.T) {
	gw := newFakeNativeGateway()
	b := newModelSetupBackend(t, gw)
	b.providers.SetVirtualKeyGateway(gw)
	ctx := context.Background()
	if _, err := b.ConnectZAI(ctx, apitypes.ConnectZAIRequest{Plan: "payg", APIKey: "sk-payg"}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	other, err := b.providers.CreateCredential(ctx, providers.CreateCredentialInput{
		Provider: providers.ProviderOpenAI, CredentialType: providers.CredentialTypeAPIKey,
		DisplayName: "keep", SecretID: "sec-keep", ManagedBy: providers.ManagedByOmahab,
	})
	if err != nil {
		t.Fatalf("unrelated credential: %v", err)
	}
	if err := b.DisconnectProvider(ctx, providers.ProviderZAI); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	creds, err := b.providers.ListCredentialsByProvider(ctx, providers.ProviderZAI)
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 0 {
		t.Fatalf("zai creds remain: %d", len(creds))
	}
	if _, err := b.providers.GetCredential(ctx, other.ID); err != nil {
		t.Fatalf("unrelated credential must survive: %v", err)
	}
}
