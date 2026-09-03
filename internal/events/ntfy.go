package events

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/secrets"
)

// NtfySink posts warning|error events to a local ntfy server at 127.0.0.1:2586
// when the admin has opted in (platform-app/ntfy_topic exists). Default is off.
type NtfySink struct {
	secrets *secrets.Service
	client  *http.Client
	baseURL string
}

// NewNtfySink creates a sink. secrets may be nil (then disabled).
func NewNtfySink(secrets *secrets.Service) *NtfySink {
	return &NtfySink{
		secrets: secrets,
		client:  &http.Client{Timeout: 5 * time.Second},
		baseURL: "http://127.0.0.1:2586",
	}
}

// SetBaseURL overrides the ntfy base URL (tests).
func (n *NtfySink) SetBaseURL(u string) { n.baseURL = strings.TrimRight(strings.TrimSpace(u), "/") }

// SetHTTPClient overrides the HTTP client (tests).
func (n *NtfySink) SetHTTPClient(c *http.Client) { n.client = c }

// IsEnabled reports whether phone notifications are enabled (topic present).
func (n *NtfySink) IsEnabled(ctx context.Context) bool {
	if n.secrets == nil {
		return false
	}
	topic, err := n.secrets.RevealByName(ctx, "platform-app", "ntfy_topic")
	if err != nil || strings.TrimSpace(topic) == "" {
		return false
	}
	return true
}

// Topic returns the configured topic, if any.
func (n *NtfySink) Topic(ctx context.Context) string {
	if n.secrets == nil {
		return ""
	}
	topic, _ := n.secrets.RevealByName(ctx, "platform-app", "ntfy_topic")
	return strings.TrimSpace(topic)
}

// ShouldNotify reports whether an event severity should be forwarded to ntfy.
// Only warning|error (and aliases warn, critical, security) are forwarded.
func ShouldNotify(severity string) bool {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "warning", "warn", "error", "critical", "security":
		return true
	default:
		return false
	}
}

// Notify posts the event to ntfy if enabled and severity is warning+.
func (n *NtfySink) Notify(ctx context.Context, ev domain.Event) error {
	if !ShouldNotify(ev.Severity) {
		return nil
	}
	if n.secrets == nil {
		return nil
	}
	topic, err := n.secrets.RevealByName(ctx, "platform-app", "ntfy_topic")
	if err != nil || strings.TrimSpace(topic) == "" {
		return nil
	}
	topic = strings.TrimSpace(topic)
	if strings.Contains(topic, "/") || strings.Contains(topic, "..") {
		return fmt.Errorf("invalid ntfy topic")
	}
	// Build ntfy request: POST /<topic> with message as body.
	// Use Title header for type, Tags, Priority.
	url := n.baseURL + "/" + topic
	body := ev.Message
	if body == "" {
		body = ev.Type
	}
	// Include data summary if present but never secrets (data already redacted by Publish).
	// The message is already safe (Publish redacts sensitive keys).
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		return err
	}
	req.Header.Set("Title", ev.Type)
	req.Header.Set("Tags", strings.ReplaceAll(ev.Type, ".", ","))
	// Priority: high for error/critical, default for warning.
	if strings.EqualFold(ev.Severity, "error") || strings.EqualFold(ev.Severity, "critical") || strings.EqualFold(ev.Severity, "security") {
		req.Header.Set("Priority", "high")
	} else {
		req.Header.Set("Priority", "default")
	}
	// Content-Type text/plain
	req.Header.Set("Content-Type", "text/plain")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy post %d", resp.StatusCode)
	}
	return nil
}

// Forwarder subscribes to the events inbox and forwards warning+ events to ntfy.
type Forwarder struct {
	events *Service
	ntfy   *NtfySink
}

// NewForwarder creates a forwarder that will forward events from svc to ntfy.
func NewForwarder(svc *Service, sink *NtfySink) *Forwarder {
	return &Forwarder{events: svc, ntfy: sink}
}

// Run subscribes and forwards until ctx cancelled. It never returns an error for ntfy POST failures.
func (f *Forwarder) Run(ctx context.Context) error {
	if f.events == nil || f.ntfy == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	ch, cancel := f.events.Subscribe(ctx, "")
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			// best-effort forward; do not block inbox
			_ = f.ntfy.Notify(ctx, ev)
		}
	}
}
