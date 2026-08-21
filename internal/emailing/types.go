package emailing

import (
	"context"
	"time"
)

// DKIMResult is returned by a pluggable verifier. The Service treats it as
// authoritative for DKIM policy decisions.
type DKIMResult struct {
	// Valid is true when a syntactically correct DKIM signature verified.
	Valid bool
	// Domain is the signing domain (d= tag), lowercased.
	Domain string
	// SignedHeaders contains lowercased header names covered by the signature.
	SignedHeaders []string
	// Aligned is true when the signing domain aligns with the From domain
	// per DMARC alignment (strict or relaxed, as implemented by the verifier).
	Aligned bool
}

// DKIMVerifier is a pluggable verifier. Implementations must not log raw
// message content and must return DKIMResult without side effects.
type DKIMVerifier interface {
	Verify(ctx context.Context, raw []byte) (DKIMResult, error)
}

// Attachment represents a single MIME attachment extracted from the message.
type Attachment struct {
	Filename    string
	ContentType string
	Data        []byte
	Size        int
}

// ParsedMessage is the normalized, untrusted view of a successfully parsed
// MIME message. All fields are untrusted and must not be executed.
type ParsedMessage struct {
	EnvelopeFrom string
	HeaderFrom   string
	FromAddress  string // lowercased addr-spec from Header From
	FromDomain   string // lowercased domain of FromAddress
	Recipient    string
	Subject      string
	TextBody     string
	HTMLBody     string
	Attachments  []Attachment
	Links        []string
	RawSize      int
	DecodedSize  int
}

// IngestRequest is the Worker-to-omahabd webhook payload after the Worker
// has attached timestamp/nonce metadata and HMAC. Raw is the exact bytes
// over which the HMAC was computed. The v1 canonical covers from/to/rawSize
// exactly as forwarded from the Email Worker; EnvelopeFrom/Recipient are
// legacy aliases for From/To and are treated identically.
type IngestRequest struct {
	Timestamp    int64  // legacy int64 seconds; if TimestampStr set, that string is canonical
	TimestampStr string // decimal string as sent by worker (preferred for HMAC)
	Nonce        string // unique per request, opaque
	Raw          []byte // raw RFC 5322 message
	Signature    string // hex-encoded HMAC-SHA256, may be "sha256=hex"
	From         string // envelope MAIL FROM (canonical)
	To           string // envelope RCPT TO (canonical)
	RawSize      int    // declared raw length; 0 means len(Raw)
	// Legacy aliases
	EnvelopeFrom string // alias for From
	Recipient    string // alias for To
}

func (r IngestRequest) canonicalTimestamp() string {
	if r.TimestampStr != "" {
		return r.TimestampStr
	}
	if r.Timestamp != 0 {
		// Use decimal without leading zeros.
		return formatInt64(r.Timestamp)
	}
	return ""
}

func (r IngestRequest) canonicalFrom() string {
	if r.From != "" {
		return r.From
	}
	return r.EnvelopeFrom
}

func (r IngestRequest) canonicalTo() string {
	if r.To != "" {
		return r.To
	}
	return r.Recipient
}

func formatInt64(v int64) string {
	// Avoid importing strconv in types.go body; use local.
	if v == 0 {
		return "0"
	}
	neg := false
	if v < 0 {
		neg = true
		v = -v
	}
	var buf [20]byte
	pos := len(buf)
	for v > 0 {
		pos--
		buf[pos] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

type IngestResult struct {
	MessageID  string
	Status     string // "received", "quarantined"
	Quarantine *QuarantineRecord
	Parsed     *ParsedMessage
}

// QuarantineRecord is persisted for messages that fail policy.
type QuarantineRecord struct {
	ID        string
	MessageID string
	Reason    string // safe, non-content-leaking reason code
	CreatedAt time.Time
}

// Sender is an enrolled allowed sender.
type Sender struct {
	ID         string
	Email      string // lowercased
	Status     string // "pending" | "verified"
	CreatedAt  time.Time
	VerifiedAt *time.Time
}

// Policy controls explicit routing for verified messages.
type Policy struct {
	StoreInbox                      bool // keep in AI inbox
	ToPaperless                     bool // route PDF attachments to Paperless
	ToKarakeep                      bool // save links to Karakeep
	QuarantineUnexpectedAttachments bool // quarantine if attachments present but ToPaperless false
}

// AttachmentRouter is invoked for each attachment of a verified message
// when Policy.ToPaperless is true or per-sender policy allows it.
// Implementations must be explicit and testable.
type AttachmentRouter interface {
	RouteAttachment(ctx context.Context, msg *ParsedMessage, att Attachment) error
}

// LinkRouter is invoked for each link of a verified message when
// Policy.ToKarakeep is true or per-sender policy allows it.
type LinkRouter interface {
	RouteLink(ctx context.Context, msg *ParsedMessage, link string) error
}

// EventSink receives normalized control-plane events.
type EventSink interface {
	Emit(ctx context.Context, event Event) error
}

// Event is a minimal normalized event emitted by the emailing controller.
type Event struct {
	Type       string         `json:"type"` // "email.received" | "email.quarantined"
	Severity   string         `json:"severity"`
	ResourceID string         `json:"resource_id,omitempty"`
	Message    string         `json:"message"`
	Data       map[string]any `json:"data,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
}

// Config controls webhook authentication and limits.
type Config struct {
	// HMACKey is the shared secret with the Email Worker. Must be non-empty
	// in production; empty key causes Ingest to reject all requests.
	HMACKey []byte
	// MaxRawBytes is the maximum raw message size. Zero means default.
	MaxRawBytes int
	// MaxDecodedBytes is the maximum sum of decoded body+attachments.
	MaxDecodedBytes int
	// ClockSkew is the maximum allowed absolute difference between
	// now and request Timestamp. Zero means default.
	ClockSkew time.Duration
	// Now is injected for testing; if nil, time.Now().UTC is used.
	Now func() time.Time
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c Config) rawLimit() int {
	if c.MaxRawBytes > 0 {
		return c.MaxRawBytes
	}
	return 5 << 20 // 5 MiB default raw
}

func (c Config) decodedLimit() int {
	if c.MaxDecodedBytes > 0 {
		return c.MaxDecodedBytes
	}
	return 10 << 20 // 10 MiB default decoded
}

func (c Config) skew() time.Duration {
	if c.ClockSkew > 0 {
		return c.ClockSkew
	}
	return 5 * time.Minute
}
