package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/emailing"
)

// The route must not be created before a sender is enrolled: enrollment is
// DESIGN §18 step 1, the route is step 2.
func TestEnsureEmailRoute_RequiresEnrolledSender(t *testing.T) {
	b, _ := newSetupBackend(t, nil)
	ctx := context.Background()
	if err := b.store.Migrate(ctx, emailing.Migrations()...); err != nil {
		t.Fatalf("migrate emailing: %v", err)
	}
	svc, err := emailing.New(b.store, emailing.Config{HMACKey: []byte("test-key-32-bytes-long-enough!!")})
	if err != nil {
		t.Fatalf("new emailing: %v", err)
	}
	b.emailing = svc
	if err := b.EnsureEmailRoute(ctx, "ai@example.com"); err == nil {
		t.Fatal("route without enrolled sender must fail")
	} else if !strings.Contains(strings.ToLower(err.Error()), "enroll") {
		t.Fatalf("error = %v, want enrollment guidance", err)
	}
	sender, err := b.EnrollEmailSender(ctx, "You@EXAMPLE.com")
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if sender.Email != "you@example.com" || sender.Status != "pending" {
		t.Fatalf("sender = %+v, want normalized pending", sender)
	}
	// Enrolled but no Token C router: the failure is about routing config,
	// not enrollment.
	if err := b.EnsureEmailRoute(ctx, "ai@example.com"); err == nil {
		t.Fatal("route without router must fail")
	} else if !strings.Contains(strings.ToLower(err.Error()), "routing not configured") {
		t.Fatalf("error = %v, want routing-config guidance", err)
	}
	senders, err := b.ListEmailSenders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(senders) != 1 || senders[0].Email != "you@example.com" {
		t.Fatalf("senders = %+v", senders)
	}
	if err := b.RemoveEmailSender(ctx, "you@example.com"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if senders, _ := b.ListEmailSenders(ctx); len(senders) != 0 {
		t.Fatalf("senders after remove = %+v", senders)
	}
}
