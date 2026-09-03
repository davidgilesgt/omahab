package secrets

import (
	"bytes"
	"testing"
)

func TestGenerateRecoveryPhrase_RoundTrip(t *testing.T) {
	words, seed, err := GenerateRecoveryPhrase()
	if err != nil {
		t.Fatalf("GenerateRecoveryPhrase: %v", err)
	}
	if len(words) != 24 {
		t.Fatalf("words len = %d, want 24", len(words))
	}
	// PhraseToSeed should recover same seed
	gotSeed, err := PhraseToSeed(words)
	if err != nil {
		t.Fatalf("PhraseToSeed: %v", err)
	}
	if gotSeed != seed {
		t.Fatalf("seed mismatch: got %x want %x", gotSeed, seed)
	}
	// Derive key deterministic
	k1 := DeriveRecoveryKey(seed)
	k2 := DeriveRecoveryKey(gotSeed)
	if k1 != k2 {
		t.Fatalf("DeriveRecoveryKey not deterministic")
	}
	if bytes.Equal(k1[:], seed[:]) {
		t.Fatalf("derived key should differ from seed")
	}
}

func TestWrapUnwrapMasterKey(t *testing.T) {
	words, seed, err := GenerateRecoveryPhrase()
	if err != nil {
		t.Fatalf("GenerateRecoveryPhrase: %v", err)
	}
	_ = words
	recKey := DeriveRecoveryKey(seed)
	var master [32]byte
	for i := range master {
		master[i] = byte(i * 7)
	}
	wrapped := WrapMasterKey(master, recKey)
	if len(wrapped) == 0 {
		t.Fatalf("wrapped empty")
	}
	// nonce||ct should be 12 + 48 (32+16) = 60? But gcm overhead 16.
	if len(wrapped) != 12+32+16 {
		t.Fatalf("wrapped len = %d, want %d", len(wrapped), 12+32+16)
	}
	unwrapped, err := UnwrapMasterKey(wrapped, recKey)
	if err != nil {
		t.Fatalf("UnwrapMasterKey: %v", err)
	}
	if unwrapped != master {
		t.Fatalf("unwrapped mismatch")
	}
}

func TestPhraseToSeed_WrongPhraseFails(t *testing.T) {
	words, _, err := GenerateRecoveryPhrase()
	if err != nil {
		t.Fatalf("GenerateRecoveryPhrase: %v", err)
	}
	// tamper one word
	bad := make([]string, len(words))
	copy(bad, words)
	// Change last word to something else that is valid BIP39 but breaks checksum
	// Use "abandon" if not already, else "zoo"
	if bad[23] == "abandon" {
		bad[23] = "zoo"
	} else {
		bad[23] = "abandon"
	}
	if _, err := PhraseToSeed(bad); err == nil {
		t.Fatalf("expected error for tampered phrase")
	}
	// Wrong length
	if _, err := PhraseToSeed(words[:12]); err == nil {
		t.Fatalf("expected error for short phrase")
	}
	// Invalid checksum with 24 words but last word wrong should fail IsMnemonicValid
}

func TestUnwrapWithWrongKeyFails(t *testing.T) {
	words, seed, err := GenerateRecoveryPhrase()
	if err != nil {
		t.Fatalf("GenerateRecoveryPhrase: %v", err)
	}
	words2, seed2, err := GenerateRecoveryPhrase()
	if err != nil {
		t.Fatalf("GenerateRecoveryPhrase second: %v", err)
	}
	_ = words
	_ = words2
	// ensure seeds differ (probabilistic, but chance collision negligible)
	if seed == seed2 {
		t.Skip("seeds collided, retry")
	}
	k1 := DeriveRecoveryKey(seed)
	k2 := DeriveRecoveryKey(seed2)
	var master [32]byte
	for i := range master {
		master[i] = 0x42
	}
	wrapped := WrapMasterKey(master, k1)
	if _, err := UnwrapMasterKey(wrapped, k2); err == nil {
		t.Fatalf("expected unwrap failure with wrong key")
	}
	// Tamper ciphertext
	tampered := make([]byte, len(wrapped))
	copy(tampered, wrapped)
	tampered[len(tampered)-1] ^= 0xFF
	if _, err := UnwrapMasterKey(tampered, k1); err == nil {
		t.Fatalf("expected unwrap failure with tampered blob")
	}
}

func TestDeriveRecoveryKey_DifferentSeedsDifferentKeys(t *testing.T) {
	_, s1, _ := GenerateRecoveryPhrase()
	_, s2, _ := GenerateRecoveryPhrase()
	if s1 == s2 {
		t.Skip("seeds equal")
	}
	k1 := DeriveRecoveryKey(s1)
	k2 := DeriveRecoveryKey(s2)
	if k1 == k2 {
		t.Fatalf("different seeds gave same derived key")
	}
}
