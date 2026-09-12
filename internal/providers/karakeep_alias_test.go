package providers

import (
	"context"
	"testing"

	"github.com/omahab/omahab/internal/store"
)

// The karakeep routing alias must pass every allowlist gate (alias set,
// reconcile, key issuance) like the four established aliases.
func TestKarakeepAliasRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx, Migrations()...); err != nil {
		t.Fatal(err)
	}
	svc := New(st.DB(), nil)
	svc.SetVirtualKeyGateway(NoopGateway{})

	if _, err := svc.CreateCredential(ctx, CreateCredentialInput{
		ID:             "cred-karakeep",
		Provider:       ProviderOpenAI,
		CredentialType: CredentialTypeAPIKey,
		DisplayName:    "karakeep test",
		SecretID:       "test-secret-id",
		ManagedBy:      ManagedByOmahab,
	}); err != nil {
		t.Fatalf("create credential: %v", err)
	}
	got, err := svc.SetAlias(ctx, SetAliasInput{
		Name:         AliasKarakeep,
		CredentialID: "cred-karakeep",
		Model:        "gpt-4o-mini",
	})
	if err != nil {
		t.Fatalf("set omahab/karakeep: %v", err)
	}
	if got.Name != AliasKarakeep || got.Model != "gpt-4o-mini" {
		t.Fatalf("alias = %+v, want karakeep/gpt-4o-mini", got)
	}
	if _, err := svc.GetAlias(ctx, AliasKarakeep); err != nil {
		t.Fatalf("get omahab/karakeep: %v", err)
	}
	vk, err := svc.IssueVirtualKey(ctx, IssueVirtualKeyInput{
		Name:   "karakeep-test",
		Scopes: []string{AliasKarakeep},
	})
	if err != nil {
		t.Fatalf("issue karakeep-scoped key: %v", err)
	}
	if vk.Token == "" {
		t.Fatal("issued token is empty")
	}
	if _, err := svc.SetAlias(ctx, SetAliasInput{
		Name:         "omahab/nope",
		CredentialID: "cred-karakeep",
		Model:        "x",
	}); err == nil {
		t.Fatal("unknown alias must still be rejected")
	}
}
