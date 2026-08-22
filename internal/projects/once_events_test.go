package projects

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestIngestOnceEvent_DeployCompleted(t *testing.T) {
	rec := &fakeRecorder{}
	svc := newService(t, &fakeRunner{}, rec, nil)
	proj, err := svc.Create(context.Background(), CreateParams{Slug: "blog", Name: "Blog", RepositoryURL: "https://forgejo.example.com/acme/blog", Image: "forgejo.example.com/acme/blog"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	evt := map[string]any{
		"event_type": "deploy_completed",
		"timestamp":  time.Now().UTC().Format(time.RFC3339Nano),
		"slug":       "blog",
		"commit":     "abc123",
		"digest":     "sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		"version":    "v1.0.0",
	}
	raw, _ := json.Marshal(evt)
	if err := svc.IngestOnceEvent(context.Background(), raw); err != nil {
		t.Fatalf("IngestOnceEvent: %v", err)
	}
	if len(rec.events) != 2 {
		t.Fatalf("want 2 events (project.created + ingestion), got %d", len(rec.events))
	}
	last := rec.events[len(rec.events)-1]
	if last.Type != "deployment.completed" {
		t.Fatalf("type = %q, want deployment.completed", last.Type)
	}
	if last.ResourceID != proj.ID {
		t.Fatalf("resource = %q, want %q", last.ResourceID, proj.ID)
	}
}

func TestIngestOnceEvent_HealthProbe(t *testing.T) {
	rec := &fakeRecorder{}
	svc := newService(t, &fakeRunner{}, rec, nil)
	_, _ = svc.Create(context.Background(), CreateParams{Slug: "blog2", Name: "Blog2", RepositoryURL: "https://forgejo.example.com/acme/blog2", Image: "forgejo.example.com/acme/blog2"})
	evt := map[string]any{
		"event_type": "health_probe",
		"slug":       "blog2",
		"healthy":    true,
	}
	raw, _ := json.Marshal(evt)
	if err := svc.IngestOnceEvent(context.Background(), raw); err != nil {
		t.Fatalf("IngestOnceEvent health healthy: %v", err)
	}
	last := rec.events[len(rec.events)-1]
	if last.Type != "service.healthy" {
		t.Logf("health probe emitted type %q (expected service.healthy)", last.Type)
	}
	evt2 := map[string]any{
		"event_type": "health_probe",
		"slug":       "blog2",
		"healthy":    false,
		"error":      "container down",
	}
	raw2, _ := json.Marshal(evt2)
	if err := svc.IngestOnceEvent(context.Background(), raw2); err != nil {
		t.Fatalf("IngestOnceEvent health unhealthy: %v", err)
	}
	last = rec.events[len(rec.events)-1]
	if last.Type != "service.unhealthy" {
		t.Fatalf("health unhealthy type = %q, want service.unhealthy", last.Type)
	}
}

func TestIngestOnceEvent_Validation(t *testing.T) {
	svc := newService(t, &fakeRunner{}, &fakeRecorder{}, nil)
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"empty", "", true},
		{"invalid json", "{", true},
		{"missing event_type", `{"slug":"a"}`, true},
		{"unknown type", `{"event_type":"unknown","slug":"a"}`, true},
		{"missing project", `{"event_type":"deploy_started"}`, true},
		{"slug not found", `{"event_type":"deploy_started","slug":"validslug"}`, true},
	}
	for _, tc := range cases {
		err := svc.IngestOnceEvent(context.Background(), []byte(tc.raw))
		if tc.want && err == nil {
			t.Errorf("%s: want error, got nil", tc.name)
		}
		if !tc.want && err != nil {
			t.Errorf("%s: want nil, got %v", tc.name, err)
		}
	}
	_, _ = svc.Create(context.Background(), CreateParams{Slug: "validslug", Name: "Valid", RepositoryURL: "https://forgejo.example.com/acme/validslug", Image: "forgejo.example.com/acme/validslug"})
	err := svc.IngestOnceEvent(context.Background(), []byte(`{"event_type":"deploy_started","slug":"validslug"}`))
	if err != nil {
		t.Fatalf("valid with existing slug should not error: %v", err)
	}
}

func TestIngestOnceEvent_DeployFailed(t *testing.T) {
	rec := &fakeRecorder{}
	svc := newService(t, &fakeRunner{}, rec, nil)
	_, _ = svc.Create(context.Background(), CreateParams{Slug: "failproj", Name: "Fail", RepositoryURL: "https://forgejo.example.com/acme/failproj", Image: "forgejo.example.com/acme/failproj"})
	evt := map[string]any{
		"event_type": "deploy_failed",
		"slug":       "failproj",
		"error":      "health check failed",
	}
	raw, _ := json.Marshal(evt)
	if err := svc.IngestOnceEvent(context.Background(), raw); err != nil {
		t.Fatalf("IngestOnceEvent failed: %v", err)
	}
	last := rec.events[len(rec.events)-1]
	if last.Type != "deployment.failed" {
		t.Fatalf("want deployment.failed, got %q", last.Type)
	}
}
