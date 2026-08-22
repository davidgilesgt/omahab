package secrets

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"strings"
	"testing"

	"filippo.io/age"
	"filippo.io/age/armor"

	_ "modernc.org/sqlite"
)

func TestGenerateAgeKeyPair(t *testing.T) {
	pub, priv, err := GenerateAgeKeyPair()
	if err != nil {
		t.Fatalf("GenerateAgeKeyPair: %v", err)
	}
	if !strings.HasPrefix(pub, "age1") {
		t.Fatalf("public key prefix = %q", pub)
	}
	if !strings.HasPrefix(strings.ToUpper(priv), "AGE-SECRET-KEY-1") {
		t.Fatalf("private key prefix = %q", priv)
	}
	// Pair must be usable.
	pt := []byte("hello omahab")
	ct, err := EncryptToAge(pt, pub)
	if err != nil {
		t.Fatalf("EncryptToAge with generated key: %v", err)
	}
	ids, err := age.ParseIdentities(strings.NewReader(priv))
	if err != nil {
		t.Fatalf("ParseIdentities: %v", err)
	}
	r := armor.NewReader(strings.NewReader(ct))
	dec, err := age.Decrypt(r, ids...)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	got, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("round trip mismatch: got %q want %q", got, pt)
	}
}

func TestEncryptToAge_RoundTrip(t *testing.T) {
	pub, priv, err := GenerateAgeKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		pt   []byte
	}{
		{"short", []byte("master key material 32 bytes long!!")},
		{"empty-adjacent", []byte("a")},
		{"binary", bytes.Repeat([]byte{0xff, 0x00, 0xab}, 20)},
		{"32bytes", bytes.Repeat([]byte("x"), 32)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ct, err := EncryptToAge(tc.pt, pub)
			if err != nil {
				t.Fatalf("EncryptToAge: %v", err)
			}
			if !strings.Contains(ct, "BEGIN AGE ENCRYPTED FILE") {
				t.Fatalf("not armored: %q", ct[:100])
			}
			got, err := decryptWithAge(ct, priv)
			if err != nil {
				t.Fatalf("decryptWithAge: %v", err)
			}
			if !bytes.Equal(got, tc.pt) {
				t.Fatalf("got %x want %x", got, tc.pt)
			}
		})
	}
}

func TestEncryptToAge_TamperFails(t *testing.T) {
	pub, priv, err := GenerateAgeKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	pt := []byte("recovery master key 32 bytes!!!!!!")
	ct, err := EncryptToAge(pt, pub)
	if err != nil {
		t.Fatal(err)
	}
	// Tamper: flip a byte in the middle of the armored payload.
	// Find a base64 character not in header/footer.
	b := []byte(ct)
	// Locate first occurrence after header line.
	idx := bytes.Index(b, []byte("\n"))
	if idx < 0 {
		t.Fatal("unexpected armor format")
	}
	// Find a position mid-file that is a base64 character.
	mid := len(b) / 2
	// Ensure we don't flip header/footer/newline.
	orig := b[mid]
	// Flip to a different base64-valid char; if it's newline, adjust.
	if b[mid] == '\n' || b[mid] == '-' {
		mid++
		orig = b[mid]
	}
	if orig == 'A' {
		b[mid] = 'B'
	} else {
		b[mid] = 'A'
	}
	tampered := string(b)
	if _, err := decryptWithAge(tampered, priv); err == nil {
		t.Fatal("tampered ciphertext decrypted without error")
	}
	// Also try truncating.
	trunc := ct[:len(ct)/2]
	if _, err := decryptWithAge(trunc, priv); err == nil {
		t.Fatal("truncated ciphertext decrypted without error")
	}
}

func TestEncryptToAge_WrongRecipientFails(t *testing.T) {
	pubA, _, err := GenerateAgeKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	_, privB, err := GenerateAgeKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	pt := []byte("super secret 32 byte master key!!")
	ct, err := EncryptToAge(pt, pubA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decryptWithAge(ct, privB); err == nil {
		t.Fatal("decryption with wrong recipient succeeded")
	}
	// Also wrong recipient string should fail at encrypt time? Test invalid.
	if _, err := EncryptToAge(pt, "age1invalidinvalidinvalidinvalidinvalidinvalidinvalidinvalid"); err == nil {
		t.Fatal("invalid recipient accepted")
	}
}

func TestEncryptToAge_InvalidRecipient(t *testing.T) {
	tests := []string{
		"",
		"   ",
		"not-a-key",
		"age1qqqq",
		"AGE-SECRET-KEY-1QL3Z7HJY54PW3HYWW5AYYFG7ZQGV...",
	}
	for _, tc := range tests {
		if _, err := EncryptToAge([]byte("x"), tc); err == nil {
			t.Errorf("EncryptToAge with recipient %q should fail", tc)
		}
	}
}

func TestEncryptToAge_HybridRecipient(t *testing.T) {
	// Generate hybrid identity and ensure EncryptToAge accepts its recipient.
	hid, err := age.GenerateHybridIdentity()
	if err != nil {
		t.Skipf("hybrid generation not supported: %v", err)
	}
	pub := hid.Recipient().String()
	priv := hid.String()
	if !strings.HasPrefix(pub, "age1pq1") {
		t.Fatalf("hybrid pub prefix = %q", pub)
	}
	pt := []byte("hybrid test master key 32 bytes...")
	ct, err := EncryptToAge(pt, pub)
	if err != nil {
		t.Fatalf("EncryptToAge hybrid: %v", err)
	}
	got, err := decryptWithAge(ct, priv)
	if err != nil {
		t.Fatalf("decrypt hybrid: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("hybrid round trip mismatch")
	}
	// Wrong hybrid recipient should fail.
	pub2, _, err := GenerateAgeKeyPair() // X25519 pub
	if err != nil {
		t.Fatal(err)
	}
	// Encrypt to X25519, try decrypt with hybrid private -> fail.
	ct2, err := EncryptToAge(pt, pub2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decryptWithAge(ct2, priv); err == nil {
		t.Fatal("X25519 ciphertext decrypted with hybrid identity")
	}
}

func TestExportRecoveryCopy(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Minimal secrets table.
	if _, err := db.Exec(`CREATE TABLE secrets (id TEXT PRIMARY KEY, scope TEXT NOT NULL, name TEXT NOT NULL, version INTEGER NOT NULL DEFAULT 1, nonce BLOB NOT NULL, ciphertext BLOB NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, UNIQUE(scope, name));`); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	svc, err := New(db, key)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	pub, priv, err := GenerateAgeKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	armored, err := svc.ExportRecoveryCopy(ctx, pub)
	if err != nil {
		t.Fatalf("ExportRecoveryCopy: %v", err)
	}
	if !strings.Contains(armored, "BEGIN AGE ENCRYPTED FILE") {
		t.Fatalf("not armored")
	}
	// Decrypt and verify it equals master key.
	plain, err := decryptWithAge(armored, priv)
	if err != nil {
		t.Fatalf("decrypt recovery copy: %v", err)
	}
	if !bytes.Equal(plain, key) {
		t.Fatalf("recovery copy mismatch: got %x want %x", plain, key)
	}
	// Empty recipient should fail validation.
	if _, err := svc.ExportRecoveryCopy(ctx, ""); err == nil {
		t.Fatal("empty recipient should fail")
	}
	// Cancelled context should fail.
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := svc.ExportRecoveryCopy(cancelCtx, pub); err == nil {
		t.Fatal("cancelled ctx should fail")
	}
}

func TestExportRecoveryCopy_RequiresSetupExport(t *testing.T) {
	// Document that setup requires the recovery export step: verify that without
	// exporting, the master key is not otherwise recoverable. This test just
	// ensures ExportRecoveryCopy is the supported path and that it produces
	// armored output distinct per recipient.
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE secrets (id TEXT PRIMARY KEY, scope TEXT NOT NULL, name TEXT NOT NULL, version INTEGER NOT NULL DEFAULT 1, nonce BLOB NOT NULL, ciphertext BLOB NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, UNIQUE(scope, name));`); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x42}, 32)
	svc, err := New(db, key)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	pub1, _, _ := mustGenerate(t)
	pub2, _, _ := mustGenerate(t)
	ctx := context.Background()
	ct1, err := svc.ExportRecoveryCopy(ctx, pub1)
	if err != nil {
		t.Fatal(err)
	}
	ct2, err := svc.ExportRecoveryCopy(ctx, pub2)
	if err != nil {
		t.Fatal(err)
	}
	if ct1 == ct2 {
		t.Fatal("recovery copies for different recipients should differ")
	}
}

func mustGenerate(t *testing.T) (string, string, error) {
	t.Helper()
	return GenerateAgeKeyPair()
}
