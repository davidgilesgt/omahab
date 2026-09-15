package emailing

import (
	"testing"
	"time"
)

// Subjects are metadata callers may display: the domain conversion must carry
// them instead of dropping them to empty.
func TestToDomain_PreservesSubject(t *testing.T) {
	now := time.Now().UTC()
	m := QuarantinedOrReceived{
		ID: "m1", EnvelopeFrom: "you@example.com", HeaderFrom: "You <you@example.com>",
		Recipient: "ai@example.com", Subject: "Hello AI",
		Authentication: "dkim=example.com", Status: StatusReceived, ReceivedAt: now,
	}
	got := m.ToDomain()
	if got.Subject != "Hello AI" {
		t.Fatalf("subject = %q", got.Subject)
	}
	if got.EnvelopeFrom != "you@example.com" || got.Recipient != "ai@example.com" || got.Status != StatusReceived {
		t.Fatalf("domain = %+v", got)
	}
	if len(DomainMessages([]QuarantinedOrReceived{m})) != 1 {
		t.Fatal("DomainMessages must map every view")
	}
}
