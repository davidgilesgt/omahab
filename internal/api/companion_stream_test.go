package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/events"
)

func TestCompanionStreamFiltersDeviceRelevant(t *testing.T) {
	backend := newRealBackend(t, nil)
	ctx := context.Background()
	_, code, err := backend.CreateCompanionEnrollment(ctx)
	if err != nil {
		t.Fatalf("create enrollment: %v", err)
	}
	resp, err := backend.EnrollCompanion(ctx, code)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	token := resp.Token
	if !strings.HasPrefix(token, "oma_dev_") {
		t.Fatalf("bad token prefix %q", token)
	}
	env := backend.EnvironmentsForTest()
	srv2, err := New(Config{
		Backend:      backend,
		Environments: env,
		BearerToken:  "test-token",
	})
	if err != nil {
		t.Fatalf("new server with env: %v", err)
	}
	ts := httptest.NewServer(srv2.Handler())
	defer ts.Close()

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	eventsCh := make(chan domain.Event, 10)
	errCh := make(chan error, 1)
	go func() {
		req, _ := http.NewRequestWithContext(streamCtx, "GET", ts.URL+"/api/v1/companion/events/stream", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "text/event-stream")
		client := &http.Client{}
		r, err := client.Do(req)
		if err != nil {
			errCh <- err
			return
		}
		defer r.Body.Close()
		if r.StatusCode != 200 {
			errCh <- &testErr{r.Status}
			return
		}
		br := bufio.NewReader(r.Body)
		var dataLines []string
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				errCh <- err
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				if len(dataLines) > 0 {
					payload := strings.Join(dataLines, "\n")
					dataLines = nil
					var ev domain.Event
					if err := json.Unmarshal([]byte(payload), &ev); err == nil {
						eventsCh <- ev
					}
				}
				continue
			}
			if strings.HasPrefix(line, "data:") {
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
	}()

	time.Sleep(200 * time.Millisecond)

	start := time.Now()
	svc := backend.EventsForTest()
	if svc == nil {
		t.Fatalf("backend events nil")
	}
	_, err = svc.Publish(ctx, events.PublishInput{
		Type:     "agent.awaiting_approval",
		Severity: "info",
		Message:  "agent needs approval",
	})
	if err != nil {
		t.Fatalf("publish allowed: %v", err)
	}
	select {
	case ev := <-eventsCh:
		elapsed := time.Since(start)
		if ev.Type != "agent.awaiting_approval" {
			t.Fatalf("got type %q want agent.awaiting_approval", ev.Type)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("delivery too slow: %v", elapsed)
		}
		t.Logf("allowed event delivered in %v", elapsed)
	case err := <-errCh:
		t.Fatalf("stream error: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for allowed event")
	}

	_, err = svc.Publish(ctx, events.PublishInput{
		Type:     "email.received",
		Severity: "info",
		Message:  "email",
	})
	if err != nil {
		t.Fatalf("publish disallowed: %v", err)
	}
	select {
	case ev := <-eventsCh:
		t.Fatalf("unexpected disallowed event %q delivered", ev.Type)
	case <-time.After(600 * time.Millisecond):
	}

	_, err = svc.Publish(ctx, events.PublishInput{
		Type:     "workspace.created",
		Severity: "info",
		Message:  "ws created",
	})
	if err != nil {
		t.Fatalf("publish workspace: %v", err)
	}
	select {
	case ev := <-eventsCh:
		if ev.Type != "workspace.created" {
			t.Fatalf("got workspace type %q", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for workspace event")
	}

	_, err = svc.Publish(ctx, events.PublishInput{
		Type:     "environment.changed",
		Severity: "info",
		Message:  "env changed",
	})
	if err != nil {
		t.Fatalf("publish env: %v", err)
	}
	select {
	case ev := <-eventsCh:
		if ev.Type != "environment.changed" {
			t.Fatalf("got env type %q", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for env event")
	}

	_, err = svc.Publish(ctx, events.PublishInput{
		Type:     "companion.revoked",
		Severity: "warning",
		Message:  "revoked",
	})
	if err != nil {
		t.Fatalf("publish revoked: %v", err)
	}
	select {
	case ev := <-eventsCh:
		if ev.Type != "companion.revoked" {
			t.Fatalf("got revoked type %q", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for revoked event")
	}
	cancel()
}

type testErr struct{ s string }

func (e *testErr) Error() string { return e.s }
