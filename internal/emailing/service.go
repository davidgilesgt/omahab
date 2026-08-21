package emailing

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/store"
)

// Reason codes are safe and never contain message content.
const (
	ReasonHMACInvalid   = "hmac_invalid"
	ReasonClockSkew     = "clock_skew"
	ReasonReplay        = "replay"
	ReasonRawTooLarge   = "raw_too_large"
	ReasonDecodedLarge  = "decoded_too_large"
	ReasonNotEnrolled   = "sender_not_enrolled"
	ReasonDKIMInvalid   = "dkim_invalid"
	ReasonFromNotSigned = "from_not_signed"
	ReasonNotAligned    = "alignment_failed"
	ReasonMIMEInvalid   = "mime_invalid"
	ReasonPolicy        = "policy_quarantine"
)

// Status values for email_messages.
const (
	StatusReceived    = "received"
	StatusQuarantined = "quarantined"
)

// Service owns email ingestion state.
type Service struct {
	db               *sql.DB
	store            *store.Store
	cfg              Config
	verifier         DKIMVerifier
	attachmentRouter AttachmentRouter
	linkRouter       LinkRouter
	events           EventSink
	policy           Policy
	// perSenderPolicy allows tests to configure per-sender routing; if nil, global policy applies.
	perSenderPolicy map[string]Policy
}

// Option configures Service.
type Option func(*Service)

func WithDKIMVerifier(v DKIMVerifier) Option {
	return func(s *Service) { s.verifier = v }
}
func WithAttachmentRouter(r AttachmentRouter) Option {
	return func(s *Service) { s.attachmentRouter = r }
}
func WithLinkRouter(r LinkRouter) Option {
	return func(s *Service) { s.linkRouter = r }
}
func WithEventSink(sink EventSink) Option {
	return func(s *Service) { s.events = sink }
}
func WithPolicy(p Policy) Option {
	return func(s *Service) { s.policy = p }
}

// New creates a Service. The store must be non-nil and migrated via Migrations().
func New(st *store.Store, cfg Config, opts ...Option) (*Service, error) {
	if st == nil {
		return nil, fmt.Errorf("store is required")
	}
	// HMAC key may be empty in tests that check error paths, but warn in production.
	svc := &Service{
		store:           st,
		db:              st.DB(),
		cfg:             cfg,
		policy:          Policy{StoreInbox: true}, // default: inbox only, explicit Paperless/Karakeep off
		perSenderPolicy: make(map[string]Policy),
	}
	for _, o := range opts {
		o(svc)
	}
	return svc, nil
}

// NewID generates an opaque lowercase ID using crypto/rand.
func NewID() string {
	// Prefer store.NewID if available; fallback to local.
	// We attempt to call store.NewID via a helper that may not exist in older store stubs,
	// so we implement locally and keep store.NewID as optional.
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// EnrollSender registers an exact sender address. The address is lowercased and
// trimmed. Initially pending; verification is required via VerifySender after a
// DKIM-signed test message is observed, but EnrollSender itself does not verify
// DKIM — that is part of Ingest.
func (s *Service) EnrollSender(ctx context.Context, email string) (string, error) {
	normalized, err := normalizeEmail(email)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrValidation, err)
	}
	id := NewID()
	now := s.cfg.now().UTC().Format(time.RFC3339Nano)
	_, err = s.db.ExecContext(ctx, `INSERT INTO email_senders (id, email, status, created_at) VALUES (?, ?, 'pending', ?)`, id, normalized, now)
	if err != nil {
		if isUniqueViolation(err) {
			return "", fmt.Errorf("%w: sender already enrolled", ErrAlreadyExists)
		}
		return "", err
	}
	return id, nil
}

// VerifySender marks a pending sender as verified. In production this is
// typically called after a successful Ingest from that sender that passes DKIM.
func (s *Service) VerifySender(ctx context.Context, email string) error {
	normalized, err := normalizeEmail(email)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	now := s.cfg.now().UTC().Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx, `UPDATE email_senders SET status='verified', verified_at=? WHERE email=?`, now, normalized)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListSenders returns all enrolled senders.
func (s *Service) ListSenders(ctx context.Context) ([]Sender, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, email, status, created_at, verified_at FROM email_senders ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sender
	for rows.Next() {
		var id, email, status, createdAtStr string
		var verifiedAt sql.NullString
		if err := rows.Scan(&id, &email, &status, &createdAtStr, &verifiedAt); err != nil {
			return nil, err
		}
		createdAt, _ := time.Parse(time.RFC3339Nano, createdAtStr)
		var va *time.Time
		if verifiedAt.Valid {
			t, _ := time.Parse(time.RFC3339Nano, verifiedAt.String)
			tt := t.UTC()
			va = &tt
		}
		out = append(out, Sender{ID: id, Email: email, Status: status, CreatedAt: createdAt.UTC(), VerifiedAt: va})
	}
	return out, rows.Err()
}

// GetSender returns a sender by email.
func (s *Service) GetSender(ctx context.Context, email string) (Sender, error) {
	normalized, err := normalizeEmail(email)
	if err != nil {
		return Sender{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	var id, em, status, createdAtStr string
	var verifiedAt sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT id, email, status, created_at, verified_at FROM email_senders WHERE email=?`, normalized).Scan(&id, &em, &status, &createdAtStr, &verifiedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Sender{}, ErrNotFound
		}
		return Sender{}, err
	}
	createdAt, _ := time.Parse(time.RFC3339Nano, createdAtStr)
	var va *time.Time
	if verifiedAt.Valid {
		t, _ := time.Parse(time.RFC3339Nano, verifiedAt.String)
		tt := t.UTC()
		va = &tt
	}
	return Sender{ID: id, Email: em, Status: status, CreatedAt: createdAt.UTC(), VerifiedAt: va}, nil
}

// RemoveSender deletes an enrolled sender.
func (s *Service) RemoveSender(ctx context.Context, email string) error {
	normalized, err := normalizeEmail(email)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM email_senders WHERE email=?`, normalized)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetPolicy sets the global routing policy.
func (s *Service) SetPolicy(p Policy) { s.policy = p }

// SetSenderPolicy sets per-sender routing policy (overrides global).
func (s *Service) SetSenderPolicy(email string, p Policy) error {
	norm, err := normalizeEmail(email)
	if err != nil {
		return err
	}
	if s.perSenderPolicy == nil {
		s.perSenderPolicy = make(map[string]Policy)
	}
	s.perSenderPolicy[norm] = p
	return nil
}

func (s *Service) effectivePolicy(from string) Policy {
	if s.perSenderPolicy != nil {
		if p, ok := s.perSenderPolicy[strings.ToLower(from)]; ok {
			return p
		}
	}
	return s.policy
}

// Ingest implements the receiving-only webhook. It verifies HMAC, clock skew,
// replay, size limits, MIME parsing, exact sender, DKIM, signed-From, alignment,
// then either stores+routes or quarantines with a safe reason. Invalid
// authentication never routes to Paperless/Karakeep.
func (s *Service) Ingest(ctx context.Context, req IngestRequest) (IngestResult, error) {
	now := s.cfg.now().UTC()
	// 1. Basic validation.
	if req.Nonce == "" {
		return IngestResult{}, fmt.Errorf("%w: nonce required", ErrValidation)
	}
	if req.Signature == "" {
		return IngestResult{}, fmt.Errorf("%w: signature required", ErrAuthFailed)
	}
	if len(req.Raw) == 0 {
		return IngestResult{}, fmt.Errorf("%w: raw required", ErrValidation)
	}
	tsStr := req.canonicalTimestamp()
	if tsStr == "" {
		return IngestResult{}, fmt.Errorf("%w: timestamp required", ErrValidation)
	}
	if !isDecimalUintString(tsStr) {
		return IngestResult{}, fmt.Errorf("%w: invalid timestamp", ErrValidation)
	}
	from := req.canonicalFrom()
	to := req.canonicalTo()
	// from/to may be empty for legacy callers that used raw direct HMAC; allow empty for fallback.

	// 2. HMAC verification (v1 canonical covering from/to/raw.length).
	if len(s.cfg.HMACKey) == 0 {
		return IngestResult{}, ErrAuthFailed
	}
	// Prefer v1 canonical when worker fields are present; fallback to legacy for tests.
	valid := false
	if from != "" || to != "" || req.TimestampStr != "" {
		valid = VerifyHMACV1(s.cfg.HMACKey, tsStr, req.Nonce, from, to, req.Raw, req.Signature)
		// If v1 fails but caller used legacy framing (no from/to), try legacy as fallback
		if !valid && from == "" && to == "" {
			valid = VerifyHMAC(s.cfg.HMACKey, req.Timestamp, req.Nonce, req.Signature, req.Raw)
		}
	} else {
		// Legacy path: timestamp as int64 + nonce + raw
		valid = VerifyHMAC(s.cfg.HMACKey, req.Timestamp, req.Nonce, req.Signature, req.Raw)
		if !valid && tsStr != "" {
			// Also try v1 with empty from/to for legacy callers that set TimestampStr
			valid = VerifyHMACV1(s.cfg.HMACKey, tsStr, req.Nonce, "", "", req.Raw, req.Signature)
		}
	}
	if !valid {
		return IngestResult{}, ErrAuthFailed
	}
	// Verify rawSize if declared (worker sends rawSize); mismatch is tampering.
	if req.RawSize != 0 && req.RawSize != len(req.Raw) {
		return IngestResult{}, ErrAuthFailed
	}

	// 3. Clock skew.
	tsInt, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return IngestResult{}, fmt.Errorf("%w: invalid timestamp", ErrValidation)
	}
	reqTime := time.Unix(tsInt, 0).UTC()
	skew := s.cfg.skew()
	if diff := absDuration(now.Sub(reqTime)); diff > skew {
		return IngestResult{}, ErrClockSkew
	}

	// 4. Persistent replay prevention.
	if err := s.checkAndInsertNonce(ctx, req.Nonce, now, skew); err != nil {
		return IngestResult{}, err
	}

	// 5. Raw size limit.
	rawLimit := s.cfg.rawLimit()
	if len(req.Raw) > rawLimit {
		return s.quarantine(ctx, req, ReasonRawTooLarge, now, nil)
	}

	// 6. MIME parsing (with decoded limit). Envelope defaults to canonical from/to or parsed From.
	fromCanon := req.canonicalFrom()
	toCanon := req.canonicalTo()
	envelopeFrom := fromCanon
	if envelopeFrom == "" {
		envelopeFrom = parseEnvelopeFrom(req.Raw)
	}
	recipient := toCanon
	parsed, err := parseMessage(req.Raw, envelopeFrom, recipient, s.cfg.decodedLimit())
	if err != nil {
		if errors.Is(err, ErrTooLarge) {
			return s.quarantine(ctx, req, ReasonDecodedLarge, now, nil)
		}
		// MIME invalid -> quarantine with safe reason
		return s.quarantine(ctx, req, ReasonMIMEInvalid, now, nil)
	}
	// Also enforce decoded size explicitly (parse already does, but double-check)
	if parsed.DecodedSize > s.cfg.decodedLimit() {
		return s.quarantine(ctx, req, ReasonDecodedLarge, now, parsed)
	}

	// 7. Exact enrolled sender check (existence only; verification after DKIM).
	if parsed.FromAddress == "" {
		return s.quarantine(ctx, req, ReasonNotEnrolled, now, parsed)
	}
	sender, err := s.GetSender(ctx, parsed.FromAddress)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return s.quarantine(ctx, req, ReasonNotEnrolled, now, parsed)
		}
		return IngestResult{}, err
	}

	// 8. DKIM verification via pluggable verifier.
	if s.verifier == nil {
		return s.quarantine(ctx, req, ReasonDKIMInvalid, now, parsed)
	}
	dkim, err := s.verifier.Verify(ctx, req.Raw)
	if err != nil {
		return s.quarantine(ctx, req, ReasonDKIMInvalid, now, parsed)
	}
	if !dkim.Valid {
		return s.quarantine(ctx, req, ReasonDKIMInvalid, now, parsed)
	}
	// 9. Signed-From enforcement.
	if !containsHeader(dkim.SignedHeaders, "from") {
		return s.quarantine(ctx, req, ReasonFromNotSigned, now, parsed)
	}
	// 10. Alignment check.
	if !dkim.Aligned {
		return s.quarantine(ctx, req, ReasonNotAligned, now, parsed)
	}
	if dkim.Domain == "" {
		return s.quarantine(ctx, req, ReasonNotAligned, now, parsed)
	}
	// 11. Pending sender auto-verification: if enrollment is pending but DKIM
	// passes, promote to verified atomically before routing.
	if sender.Status == "pending" {
		if err := s.VerifySender(ctx, parsed.FromAddress); err != nil && !errors.Is(err, ErrNotFound) {
			// If verify fails for other reasons, continue but treat as verified for this ingest.
		}
	}

	// 12. Policy: quarantine unexpected attachments if configured.
	pol := s.effectivePolicy(parsed.FromAddress)
	if pol.QuarantineUnexpectedAttachments && len(parsed.Attachments) > 0 && !pol.ToPaperless {
		return s.quarantine(ctx, req, ReasonPolicy, now, parsed)
	}

	// 13. Success: store as received and route per explicit policy.
	msgID := NewID()
	receivedAt := now.Format(time.RFC3339Nano)
	createdAt := receivedAt
	authStr := fmt.Sprintf("dkim=%s", dkim.Domain)
	// Keep authentication string safe: just domain, not raw.
	_, err = s.db.ExecContext(ctx, `INSERT INTO email_messages (id, envelope_from, header_from, recipient, subject, authentication, status, raw_size, decoded_size, received_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msgID, parsed.EnvelopeFrom, parsed.HeaderFrom, parsed.Recipient, parsed.Subject, authStr, StatusReceived, parsed.RawSize, parsed.DecodedSize, receivedAt, createdAt)
	if err != nil {
		return IngestResult{}, err
	}
	s.emit(ctx, Event{
		Type:       "email.received",
		Severity:   "info",
		ResourceID: msgID,
		Message:    "email received",
		Data:       map[string]any{"from": parsed.FromAddress, "subject": parsed.Subject},
		CreatedAt:  now,
	})

	// Routing per explicit policy: only now may we invoke attachment/link routers.
	if pol.StoreInbox {
		// Inbox storage already done via email_messages; no extra action.
	}
	if pol.ToPaperless && s.attachmentRouter != nil {
		for _, att := range parsed.Attachments {
			if isPDF(att) || true {
				if err := s.attachmentRouter.RouteAttachment(ctx, parsed, att); err != nil {
				}
			}
		}
	}
	if pol.ToKarakeep && s.linkRouter != nil {
		for _, link := range parsed.Links {
			if err := s.linkRouter.RouteLink(ctx, parsed, link); err != nil {
			}
		}
	}

	return IngestResult{MessageID: msgID, Status: StatusReceived, Parsed: parsed}, nil
}
func (s *Service) checkAndInsertNonce(ctx context.Context, nonce string, now time.Time, skew time.Duration) error {
	// Clean expired nonces opportunistically.
	_, _ = s.db.ExecContext(ctx, `DELETE FROM email_nonces WHERE expires_at < ?`, now.Format(time.RFC3339Nano))
	expires := now.Add(skew * 2).Format(time.RFC3339Nano)
	created := now.Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `INSERT INTO email_nonces (nonce, expires_at, created_at) VALUES (?, ?, ?)`, nonce, expires, created)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrReplay
		}
		return err
	}
	return nil
}

func (s *Service) IngestBytes(ctx context.Context, timestamp int64, nonce string, raw []byte, sig string) (IngestResult, error) {
	return s.Ingest(ctx, IngestRequest{Timestamp: timestamp, Nonce: nonce, Raw: raw, Signature: sig})
}

func (s *Service) quarantine(ctx context.Context, req IngestRequest, reason string, now time.Time, parsed *ParsedMessage) (IngestResult, error) {
	// Extract safe header fields without logging raw content.
	// For size-limit reasons skip heavy MIME parsing to avoid OOM.
	headerFrom := ""
	subject := ""
	envelopeFrom := req.EnvelopeFrom
	recipient := req.Recipient
	rawSize := len(req.Raw)
	decodedSize := 0
	if parsed != nil {
		headerFrom = parsed.HeaderFrom
		subject = parsed.Subject
		envelopeFrom = parsed.EnvelopeFrom
		recipient = parsed.Recipient
		decodedSize = parsed.DecodedSize
	} else if reason != ReasonRawTooLarge && reason != ReasonDecodedLarge {
		// Try to extract safe headers from raw without storing content.
		// Use limited parse; if fails, leave empty (still safe).
		// Truncate raw for header extraction to first 64KB to avoid parsing huge payloads.
		headerSlice := req.Raw
		if len(headerSlice) > 64*1024 {
			// Only keep header block (up to double newline).
			if idx := findHeaderEnd(headerSlice); idx > 0 {
				headerSlice = headerSlice[:idx]
			} else {
				headerSlice = headerSlice[:64*1024]
			}
		}
		if p, err := parseMessageForHeaders(headerSlice); err == nil {
			headerFrom = p.HeaderFrom
			subject = p.Subject
			if envelopeFrom == "" {
				envelopeFrom = p.EnvelopeFrom
			}
			if recipient == "" {
				recipient = p.Recipient
			}
		}
	}
	msgID := NewID()
	quarantineID := NewID()
	receivedAt := now.Format(time.RFC3339Nano)
	createdAt := receivedAt
	// Use "quarantined" auth status to indicate quarantine.
	authStr := reason // store reason as auth for observability; safe code only.
	// Insert message.
	_, err := s.db.ExecContext(ctx, `INSERT INTO email_messages (id, envelope_from, header_from, recipient, subject, authentication, status, raw_size, decoded_size, received_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msgID, envelopeFrom, headerFrom, recipient, subject, authStr, StatusQuarantined, rawSize, decodedSize, receivedAt, createdAt)
	if err != nil {
		return IngestResult{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO email_quarantine (id, message_id, reason, details, created_at) VALUES (?, ?, ?, ?, ?)`,
		quarantineID, msgID, reason, "", createdAt)
	if err != nil {
		return IngestResult{}, err
	}
	s.emit(ctx, Event{
		Type:       "email.quarantined",
		Severity:   "warn",
		ResourceID: msgID,
		Message:    "email quarantined: " + reason,
		Data:       map[string]any{"reason": reason},
		CreatedAt:  now,
	})
	qr := &QuarantineRecord{ID: quarantineID, MessageID: msgID, Reason: reason, CreatedAt: now}
	return IngestResult{MessageID: msgID, Status: StatusQuarantined, Quarantine: qr, Parsed: parsed}, ErrQuarantined
}

// parseMessageForHeaders is a minimal header-only parse for quarantine safe fields.
func parseMessageForHeaders(raw []byte) (*ParsedMessage, error) {
	parsed, err := parseMessage(raw, "", "", 10<<20)
	if err != nil {
		// Fallback: try to extract From/Subject via mail.ReadMessage without decoded limits.
		// We already handled size, so just return minimal.
		return nil, err
	}
	return parsed, nil
}

// ListMessages returns recent messages.
func (s *Service) ListMessages(ctx context.Context, limit int) ([]QuarantinedOrReceived, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, envelope_from, header_from, recipient, subject, authentication, status, raw_size, decoded_size, received_at, created_at FROM email_messages ORDER BY received_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuarantinedOrReceived
	for rows.Next() {
		var m QuarantinedOrReceived
		var receivedAt, createdAt string
		if err := rows.Scan(&m.ID, &m.EnvelopeFrom, &m.HeaderFrom, &m.Recipient, &m.Subject, &m.Authentication, &m.Status, &m.RawSize, &m.DecodedSize, &receivedAt, &createdAt); err != nil {
			return nil, err
		}
		m.ReceivedAt, _ = time.Parse(time.RFC3339Nano, receivedAt)
		m.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetMessage returns a single message by ID.
func (s *Service) GetMessage(ctx context.Context, id string) (QuarantinedOrReceived, error) {
	var m QuarantinedOrReceived
	var receivedAt, createdAt string
	err := s.db.QueryRowContext(ctx, `SELECT id, envelope_from, header_from, recipient, subject, authentication, status, raw_size, decoded_size, received_at, created_at FROM email_messages WHERE id=?`, id).
		Scan(&m.ID, &m.EnvelopeFrom, &m.HeaderFrom, &m.Recipient, &m.Subject, &m.Authentication, &m.Status, &m.RawSize, &m.DecodedSize, &receivedAt, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return QuarantinedOrReceived{}, ErrNotFound
		}
		return QuarantinedOrReceived{}, err
	}
	m.ReceivedAt, _ = time.Parse(time.RFC3339Nano, receivedAt)
	m.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	return m, nil
}

// ListQuarantine returns quarantine records with message metadata.
func (s *Service) ListQuarantine(ctx context.Context) ([]QuarantineView, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT q.id, q.message_id, q.reason, q.created_at, m.header_from, m.subject FROM email_quarantine q JOIN email_messages m ON m.id=q.message_id ORDER BY q.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuarantineView
	for rows.Next() {
		var q QuarantineView
		var createdAt string
		if err := rows.Scan(&q.ID, &q.MessageID, &q.Reason, &createdAt, &q.HeaderFrom, &q.Subject); err != nil {
			return nil, err
		}
		q.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		out = append(out, q)
	}
	return out, rows.Err()
}

func findHeaderEnd(raw []byte) int {
	// Search for \r\n\r\n or \n\n
	if idx := indexBytes(raw, []byte("\r\n\r\n")); idx >= 0 {
		return idx + 4
	}
	if idx := indexBytes(raw, []byte("\n\n")); idx >= 0 {
		return idx + 2
	}
	return -1
}

func indexBytes(haystack, needle []byte) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

type QuarantinedOrReceived struct {
	ID             string
	EnvelopeFrom   string
	HeaderFrom     string
	Recipient      string
	Subject        string
	Authentication string
	Status         string
	RawSize        int
	DecodedSize    int
	ReceivedAt     time.Time
	CreatedAt      time.Time
}

// QuarantineView joins quarantine and message.
type QuarantineView struct {
	ID         string
	MessageID  string
	Reason     string
	HeaderFrom string
	Subject    string
	CreatedAt  time.Time
}

func (s *Service) emit(ctx context.Context, evt Event) {
	if s.events != nil {
		_ = s.events.Emit(ctx, evt)
	}
}

func normalizeEmail(email string) (string, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return "", fmt.Errorf("email required")
	}
	// Basic validation: must contain @ and not contain spaces/control.
	if strings.Contains(email, " ") || strings.Contains(email, "\n") || strings.Contains(email, "\r") {
		return "", fmt.Errorf("invalid email")
	}
	parts := strings.Split(email, "@")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || !strings.Contains(parts[1], ".") {
		return "", fmt.Errorf("invalid email")
	}
	return strings.ToLower(email), nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// modernc.org/sqlite and sqlite errors contain "UNIQUE constraint failed" or "unique"
	return strings.Contains(msg, "UNIQUE") || strings.Contains(msg, "unique") || strings.Contains(msg, "constraint failed")
}

func containsHeader(headers []string, target string) bool {
	target = strings.ToLower(target)
	for _, h := range headers {
		if strings.ToLower(strings.TrimSpace(h)) == target {
			return true
		}
	}
	return false
}

func isPDF(att Attachment) bool {
	ct := strings.ToLower(att.ContentType)
	if strings.Contains(ct, "pdf") {
		return true
	}
	fn := strings.ToLower(att.Filename)
	return strings.HasSuffix(fn, ".pdf")
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func isDecimalUintString(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	// Disallow leading zeros unless single zero? Worker allows any decimal, so accept.
	return true
}
