package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/store"
)
func TestCloudflareAccountIDValidation_RejectedEmail(t *testing.T) {
	b, _ := newSetupBackend(t, nil)
	ctx := context.Background()
	_, err := b.CreateSecret(ctx, apitypes.CreateSecretRequest{Scope: "platform-app", Name: "cloudflare_account_id", Value: "user@example.com"})
	if err == nil {
		t.Fatalf("expected validation error for email")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "sidebar") || !strings.Contains(err.Error(), "32") {
		t.Fatalf("error should point at dashboard sidebar Account ID (32 hex), got %q", err.Error())
	}
	// Also test @ in string
	_, err = b.secrets.Put(ctx, "platform-app", "cloudflare_account_id", "test@cloudflare.com")
	if err == nil || !strings.Contains(err.Error(), "@") {
		t.Fatalf("direct Put should reject email containing @, got %v", err)
	}
}

func TestCloudflareAccountIDValidation_Accepted32Hex(t *testing.T) {
	b, _ := newSetupBackend(t, nil)
	ctx := context.Background()
	validLower := "9b1a2c3d4e5f6a7b8c9d0e1f2a3b4c5d"
	validUpper := "9B1A2C3D4E5F6A7B8C9D0E1F2A3B4C5D"
	validMixed := "Ab1234567890abcdef1234567890AbCd"
	for _, v := range []string{validLower, validUpper, validMixed} {
		if len(v) != 32 {
			t.Fatalf("test value length not 32: %q", v)
		}
		sec, err := b.CreateSecret(ctx, apitypes.CreateSecretRequest{Scope: "platform-app", Name: "cloudflare_account_id", Value: v})
		if err != nil {
			t.Fatalf("valid 32-hex %q should be accepted, got %v", v, err)
		}
		if sec.Name != "cloudflare_account_id" {
			t.Fatalf("unexpected name %q", sec.Name)
		}
		plain, err := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_account_id")
		if err != nil {
			t.Fatalf("reveal after valid put: %v", err)
		}
		if plain != v {
			t.Fatalf("reveal mismatch: got %q want %q", plain, v)
		}
	}
	// Direct secrets validation also accepts
	_, err := b.secrets.Put(ctx, "platform-app", "cloudflare_account_id", validLower)
	if err != nil {
		t.Fatalf("secrets.Put valid should succeed: %v", err)
	}
}

func TestCloudflareAccountIDValidation_OverwriteWorks(t *testing.T) {
	b, _ := newSetupBackend(t, nil)
	ctx := context.Background()
	first := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	second := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	// First write via CreateSecret
	s1, err := b.CreateSecret(ctx, apitypes.CreateSecretRequest{Scope: "platform-app", Name: "cloudflare_account_id", Value: first})
	if err != nil {
		t.Fatalf("first put: %v", err)
	}
	if s1.Version != 1 {
		t.Fatalf("first version expected 1 got %d", s1.Version)
	}
	// Overwrite via same API (upsert) should succeed and bump version
	s2, err := b.CreateSecret(ctx, apitypes.CreateSecretRequest{Scope: "platform-app", Name: "cloudflare_account_id", Value: second})
	if err != nil {
		t.Fatalf("overwrite via CreateSecret should succeed (upsert), got %v", err)
	}
	if s2.Version != 2 {
		t.Fatalf("overwrite version expected 2 got %d", s2.Version)
	}
	plain, err := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_account_id")
	if err != nil {
		t.Fatalf("reveal after overwrite: %v", err)
	}
	if plain != second {
		t.Fatalf("overwrite reveal mismatch: got %q want %q", plain, second)
	}
	// Also test direct secrets Put upsert for generic secret (not just cloudflare)
	genericFirst := "first-value"
	genericSecond := "second-value"
	if _, err := b.secrets.Put(ctx, "platform-app", "cloudflare_dns", genericFirst); err != nil {
		t.Fatalf("generic first Put: %v", err)
	}
	sec2, err := b.secrets.Put(ctx, "platform-app", "cloudflare_dns", genericSecond)
	if err != nil {
		t.Fatalf("generic overwrite Put should succeed (upsert): %v", err)
	}
	if sec2.Version != 2 {
		t.Fatalf("generic overwrite version expected 2 got %d", sec2.Version)
	}
	plain2, err := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_dns")
	if err != nil {
		t.Fatalf("reveal generic: %v", err)
	}
	if plain2 != genericSecond {
		t.Fatalf("generic overwrite reveal mismatch: got %q want %q", plain2, genericSecond)
	}
}
func TestCloudflareAccountIDValidation_InvalidFormats(t *testing.T) {
	b, _ := newSetupBackend(t, nil)
	ctx := context.Background()
	cases := []string{
		"short",
		"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
		"9b1a2c3d4e5f6a7b8c9d0e1f2a3b4c",
		"9b1a2c3d4e5f6a7b8c9d0e1f2a3b4c5dX",
		"not-an-email-but-invalid-hex!!!!",
	}
	for _, tc := range cases {
		_, err := b.secrets.Put(ctx, "platform-app", "cloudflare_account_id", tc)
		if err == nil {
			t.Fatalf("expected validation error for %q", tc)
		}
		if !errors.Is(err, store.ErrValidation) {
			t.Fatalf("expected validation error for %q, got %v", tc, err)
		}
	}
}
