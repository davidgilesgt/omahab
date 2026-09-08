package diskinstall

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/omahab/omahab/internal/sshkeys"
	"golang.org/x/crypto/ssh"
)

func mustTestKey(t *testing.T) sshkeys.SSHKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatalf("new pub: %v", err)
	}
	line := string(ssh.MarshalAuthorizedKey(pub))
	// Trim newline and add comment
	line = line[:len(line)-1] + " test@example.com"
	k, err := sshkeys.ParseAuthorizedKeysLine(line, "test")
	if err != nil {
		t.Fatalf("parse test key %q: %v", line, err)
	}
	return *k
}

func TestValidateSSHChoice(t *testing.T) {
	if err := ValidateSSHChoice(SSHChoice{Mode: "defer"}); err != nil {
		t.Fatalf("defer: %v", err)
	}
	if err := ValidateSSHChoice(SSHChoice{Mode: "github", GithubUser: "octocat", Keys: []sshkeys.SSHKey{mustTestKey(t)}}); err != nil {
		t.Fatalf("github: %v", err)
	}
	if err := ValidateSSHChoice(SSHChoice{Mode: "github", GithubUser: ""}); err == nil {
		t.Fatal("github without user should fail")
	}
	if err := ValidateSSHChoice(SSHChoice{Mode: "paste", Keys: []sshkeys.SSHKey{mustTestKey(t)}}); err != nil {
		t.Fatalf("paste: %v", err)
	}
	if err := ValidateSSHChoice(SSHChoice{Mode: "paste"}); err == nil {
		t.Fatal("paste without keys should fail")
	}
	if err := ValidateSSHChoice(SSHChoice{Mode: "invalid"}); err == nil {
		t.Fatal("invalid mode should fail")
	}
}

func TestParsePastedKeys_ValidAndInvalid(t *testing.T) {
	valid := mustTestKey(t).Raw
	// Valid paste
	keys, err := ParsePastedKeys(valid + "\n")
	if err != nil {
		t.Fatalf("parse pasted valid: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("len=%d want 1", len(keys))
	}
	// Invalid pasted
	_, err = ParsePastedKeys("invalid-key-data\n")
	if err == nil {
		t.Fatal("invalid pasted should fail")
	}
	// Empty should fail
	_, err = ParsePastedKeys("\n\n")
	if err == nil {
		t.Fatal("empty pasted should fail")
	}
}

func TestFormatKeyShort(t *testing.T) {
	k := mustTestKey(t)
	s := FormatKeyShort(k)
	if s == "" {
		t.Fatal("format empty")
	}
}
