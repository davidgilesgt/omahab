package emailing

import (
	"context"
	"sync"
)

// RecordingAttachmentRouter records routed attachments for tests.
type RecordingAttachmentRouter struct {
	mu    sync.Mutex
	Calls []AttachmentCall
	Err   error
}

type AttachmentCall struct {
	Msg *ParsedMessage
	Att Attachment
}

func (r *RecordingAttachmentRouter) RouteAttachment(_ context.Context, msg *ParsedMessage, att Attachment) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Calls = append(r.Calls, AttachmentCall{Msg: msg, Att: att})
	return r.Err
}

func (r *RecordingAttachmentRouter) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.Calls)
}

// RecordingLinkRouter records routed links.
type RecordingLinkRouter struct {
	mu    sync.Mutex
	Links []LinkCall
	Err   error
}

type LinkCall struct {
	Msg  *ParsedMessage
	Link string
}

func (r *RecordingLinkRouter) RouteLink(_ context.Context, msg *ParsedMessage, link string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Links = append(r.Links, LinkCall{Msg: msg, Link: link})
	return r.Err
}

func (r *RecordingLinkRouter) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.Links)
}

// RecordingEventSink records emitted events.
type RecordingEventSink struct {
	mu     sync.Mutex
	Events []Event
	Err    error
}

func (s *RecordingEventSink) Emit(_ context.Context, evt Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Events = append(s.Events, evt)
	return s.Err
}

func (s *RecordingEventSink) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Events)
}

func (s *RecordingEventSink) FindByType(t string) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Event
	for _, e := range s.Events {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

// NopEventSink discards events.
type NopEventSink struct{}

func (NopEventSink) Emit(_ context.Context, _ Event) error { return nil }

// NopAttachmentRouter discards attachments.
type NopAttachmentRouter struct{}

func (NopAttachmentRouter) RouteAttachment(_ context.Context, _ *ParsedMessage, _ Attachment) error {
	return nil
}

// NopLinkRouter discards links.
type NopLinkRouter struct{}

func (NopLinkRouter) RouteLink(_ context.Context, _ *ParsedMessage, _ string) error { return nil }
