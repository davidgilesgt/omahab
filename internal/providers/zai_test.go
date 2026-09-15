package providers

import (
	"context"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/store"
)

func zaiTestService(t *testing.T) *Service {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx, Migrations()...); err != nil {
		t.Fatal(err)
	}
	return New(st.DB(), nil)
}

// Unauthorized Coding Plan saves fail; PAYG is explicit.
func TestZAICodingRequiresAck(t *testing.T) {
	svc := zaiTestService(t)
	ctx := context.Background()
	_, err := svc.CreateCredential(ctx, CreateCredentialInput{
		Provider: ProviderZAI, CredentialType: CredentialTypeAPIKey,
		DisplayName: "z", SecretID: "sec-1", ManagedBy: ManagedByOmahab,
		Plan: ZAIPlanCoding,
	})
	if err == nil || !strings.Contains(err.Error(), "acknowledgement") {
		t.Fatalf("coding without ack = %v, want acknowledgement error", err)
	}
	cred, err := svc.CreateCredential(ctx, CreateCredentialInput{
		Provider: ProviderZAI, CredentialType: CredentialTypeAPIKey,
		DisplayName: "z", SecretID: "sec-1", ManagedBy: ManagedByOmahab,
		Plan: ZAIPlanCoding, AuthorizedCodingUse: true,
	})
	if err != nil {
		t.Fatalf("coding with ack: %v", err)
	}
	if !cred.AuthorizedCodingUse || cred.AuthorizedAt == nil {
		t.Fatalf("ack must be recorded with timestamp: %+v", cred)
	}
	if cred.Plan != ZAIPlanCoding {
		t.Fatalf("plan = %q", cred.Plan)
	}
	// Downgrading to an unknown plan fails.
	bad := "ultra"
	if _, err := svc.UpdateCredential(ctx, cred.ID, UpdateCredentialInput{Plan: &bad}); err == nil {
		t.Fatal("unknown plan must fail")
	}
	// Switching to PAYG keeps the ack history but stops requiring it.
	payg := ZAIPlanPAYG
	updated, err := svc.UpdateCredential(ctx, cred.ID, UpdateCredentialInput{Plan: &payg})
	if err != nil {
		t.Fatalf("plan change: %v", err)
	}
	if updated.Plan != ZAIPlanPAYG {
		t.Fatalf("plan = %q", updated.Plan)
	}
	// Re-entering coding without ack fails even with history.
	if _, err := svc.UpdateCredential(ctx, cred.ID, UpdateCredentialInput{Plan: &[]string{ZAIPlanCoding}[0]}); err == nil {
		t.Fatal("coding without fresh ack must fail")
	}
}

func TestZAIRequiresExplicitPlan(t *testing.T) {
	svc := zaiTestService(t)
	ctx := context.Background()
	cred, err := svc.CreateCredential(ctx, CreateCredentialInput{
		Provider: ProviderZAI, CredentialType: CredentialTypeAPIKey,
		DisplayName: "z", SecretID: "sec-1", ManagedBy: ManagedByOmahab,
	})
	if err != nil {
		t.Fatalf("default plan: %v", err)
	}
	if cred.Plan != ZAIPlanPAYG {
		t.Fatalf("default plan = %q, want explicit payg", cred.Plan)
	}
}

func TestZAIPlanRejectedForOthers(t *testing.T) {
	svc := zaiTestService(t)
	ctx := context.Background()
	_, err := svc.CreateCredential(ctx, CreateCredentialInput{
		Provider: ProviderOpenAI, CredentialType: CredentialTypeAPIKey,
		DisplayName: "o", SecretID: "sec-1", ManagedBy: ManagedByOmahab,
		Plan: ZAIPlanCoding, AuthorizedCodingUse: true,
	})
	if err == nil {
		t.Fatal("plan on non-zai provider must fail")
	}
}

func TestNormalizeZAI(t *testing.T) {
	got, useOAuth, responses, err := NormalizeNativeModel(ProviderZAI, CredentialTypeAPIKey, ManagedByOmahab, "", "glm-4.6")
	if err != nil {
		t.Fatal(err)
	}
	if got != "openai/glm-4.6" || useOAuth || responses {
		t.Fatalf("render = %q oauth=%v responses=%v", got, useOAuth, responses)
	}
	got, _, _, err = NormalizeNativeModel(ProviderZAI, CredentialTypeAPIKey, ManagedByOmahab, "", "zai/glm-4.6")
	if err != nil || got != "openai/glm-4.6" {
		t.Fatalf("zai prefix render = %q err = %v", got, err)
	}
}
