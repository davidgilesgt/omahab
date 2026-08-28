package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

// EventSink is the package-local publication interface used by upstream
// controllers. Implementations must not log raw secrets and must treat
// Data as untrusted.
type EventSink interface {
	Emit(ctx context.Context, ev domain.Event) error
}

// Allowed event types (DESIGN §20). Validation rejects unknown types to keep
// the inbox normalized and prevent unbounded growth.
var allowedTypes = map[string]bool{
	"backup.failed":                true,
	"backup.restored":              true,
	"backup.created":               true,
	"host.disk_low":                true,
	"service.unhealthy":            true,
	"service.update_available":     true,
	"ci.failed":                    true,
	"deployment.completed":         true,
	"agent.awaiting_approval":      true,
	"syncthing.conflict":           true,
	"syncthing.device_stale":       true,
	"email.received":               true,
	"email.quarantined":            true,
	"health.degraded":              true,
	"health.unhealthy":             true,
	"identity.recovery":            true,
	"applications.catalog_missing": true,
	"setup.step_failed":            true,
	"setup.reconciled":             true,
}

// Allowed severities.
var allowedSeverities = map[string]bool{
	"info":     true,
	"warning":  true,
	"warn":     true,
	"error":    true,
	"success":  true,
	"critical": true,
	"security": true,
}

var sensitiveKeys = []string{
	"token", "secret", "password", "passwd", "credential", "auth", "private", "key", "hmac", "signature",
}

const (
	maxMessageLen  = 2000
	maxDataBytes   = 16 * 1024
	maxSubscribers = 64
	subscriberBuf  = 32
)

// PublishInput is the validated input to create an event.
type PublishInput struct {
	Type           string
	Severity       string
	ResourceID     string
	Message        string
	Data           map[string]any
	IdempotencyKey string
}

// ListFilter controls filtered listing.
type ListFilter struct {
	Type     string
	Severity string
	Unread   *bool
}

// ListOptions controls cursor-based listing.
type ListOptions struct {
	Cursor string // opaque event ID; return events after this ID (exclusive)
	Limit  int
	Filter ListFilter
}

// Service owns the durable event inbox with fanout.
type Service struct {
	db  *sql.DB
	now func() time.Time

	mu   sync.Mutex
	subs map[int]chan domain.Event
	next int
}

// New creates a Service. db must be opened; now may be nil (defaults to time.Now UTC).
func New(db *sql.DB, now func() time.Time) *Service {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		db:   db,
		now:  now,
		subs: make(map[int]chan domain.Event),
	}
}

// Publish validates, redacts, persists durably, and fans out. It is idempotent
// when IdempotencyKey is non-empty: the first persisted event is returned for
// subsequent calls with the same key.
func (s *Service) Publish(ctx context.Context, in PublishInput) (*domain.Event, error) {
	t := strings.TrimSpace(in.Type)
	sev := strings.TrimSpace(in.Severity)
	msg := strings.TrimSpace(in.Message)
	key := strings.TrimSpace(in.IdempotencyKey)

	if t == "" {
		return nil, store.Validation("event type is required")
	}
	if !allowedTypes[t] {
		return nil, store.Validationf("unknown event type %q", t)
	}
	if sev == "" {
		sev = "info"
	}
	if !allowedSeverities[sev] {
		return nil, store.Validationf("unknown severity %q", sev)
	}
	if msg == "" {
		return nil, store.Validation("event message is required")
	}
	if len(msg) > maxMessageLen {
		msg = msg[:maxMessageLen]
	}
	rid := strings.TrimSpace(in.ResourceID)

	data := redactData(in.Data)
	if data != nil {
		if b, err := json.Marshal(data); err == nil && len(b) > maxDataBytes {
			data = truncateData(data, maxDataBytes)
		}
	}
	dataJSON := "{}"
	if data != nil && len(data) > 0 {
		b, _ := json.Marshal(data)
		dataJSON = string(b)
	}

	if key != "" {
		ev, err := s.getByIdempotency(ctx, key)
		if err == nil && ev != nil {
			return ev, nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) && !isNotFound(err) {
			return nil, err
		}
	}

	id := store.NewID()
	created := s.now().UTC().Format(time.RFC3339Nano)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO events (id, type, severity, resource_id, message, data, idempotency_key, read_at, created_at)
         VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?)
         ON CONFLICT(id) DO NOTHING`,
		id, t, sev, rid, msg, dataJSON, nullableString(key), created)
	if err != nil {
		_ = tx.Rollback()
		if isUniqueViolation(err) && key != "" {
			ev, gerr := s.getByIdempotency(ctx, key)
			if gerr == nil {
				return ev, nil
			}
			return nil, gerr
		}
		return nil, fmt.Errorf("insert event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		if isUniqueViolation(err) && key != "" {
			ev, gerr := s.getByIdempotency(ctx, key)
			if gerr == nil {
				return ev, nil
			}
			return nil, gerr
		}
		return nil, fmt.Errorf("commit event: %w", err)
	}

	ev := &domain.Event{
		ID:         domain.ID(id),
		Type:       t,
		Severity:   sev,
		ResourceID: domain.ID(rid),
		Message:    msg,
		Data:       data,
		CreatedAt:  parseTime(created),
	}
	s.broadcast(*ev)
	return ev, nil
}

// Emit implements EventSink for narrow upstream interfaces.
func (s *Service) Emit(ctx context.Context, ev domain.Event) error {
	in := PublishInput{
		Type:       ev.Type,
		Severity:   ev.Severity,
		ResourceID: string(ev.ResourceID),
		Message:    ev.Message,
		Data:       ev.Data,
	}
	_, err := s.Publish(ctx, in)
	return err
}

// Get returns a single event by ID.
func (s *Service) Get(ctx context.Context, id domain.ID) (*domain.Event, error) {
	if strings.TrimSpace(string(id)) == "" {
		return nil, store.Validation("event id is required")
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT id, type, severity, resource_id, message, data, read_at, created_at FROM events WHERE id = ?`, string(id))
	return scanEvent(row)
}

// List returns events ordered by created_at ASC, id ASC for cursor stability.
func (s *Service) List(ctx context.Context, opts ListOptions) ([]domain.Event, string, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}

	var cursorTime string
	var cursorID string
	if opts.Cursor != "" {
		ev, err := s.Get(ctx, domain.ID(opts.Cursor))
		if err != nil {
			if isNotFound(err) {
				// fall through as if no cursor
			} else {
				return nil, "", err
			}
		} else {
			cursorTime = ev.CreatedAt.UTC().Format(time.RFC3339Nano)
			cursorID = string(ev.ID)
		}
	}

	var sb strings.Builder
	args := []any{}
	sb.WriteString(`SELECT id, type, severity, resource_id, message, data, read_at, created_at FROM events WHERE 1=1`)

	if cursorTime != "" {
		sb.WriteString(` AND ((created_at > ?) OR (created_at = ? AND id > ?))`)
		args = append(args, cursorTime, cursorTime, cursorID)
	}
	if opts.Filter.Type != "" {
		if !allowedTypes[opts.Filter.Type] {
			return nil, "", store.Validationf("unknown event type %q", opts.Filter.Type)
		}
		sb.WriteString(` AND type = ?`)
		args = append(args, opts.Filter.Type)
	}
	if opts.Filter.Severity != "" {
		if !allowedSeverities[opts.Filter.Severity] {
			return nil, "", store.Validationf("unknown severity %q", opts.Filter.Severity)
		}
		sb.WriteString(` AND severity = ?`)
		args = append(args, opts.Filter.Severity)
	}
	if opts.Filter.Unread != nil {
		if *opts.Filter.Unread {
			sb.WriteString(` AND read_at IS NULL`)
		} else {
			sb.WriteString(` AND read_at IS NOT NULL`)
		}
	}
	sb.WriteString(` ORDER BY created_at ASC, id ASC LIMIT ?`)
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, "", fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	var out []domain.Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, *ev)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	var nextCursor string
	if len(out) > limit {
		nextCursor = string(out[limit-1].ID)
		out = out[:limit]
	} else if len(out) > 0 {
		nextCursor = string(out[len(out)-1].ID)
	}
	return out, nextCursor, nil
}

// ListSimple satisfies offset pagination for api.Backend compatibility.
func (s *Service) ListSimple(ctx context.Context, limit, offset int, filter ListFilter) ([]domain.Event, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	var sb strings.Builder
	args := []any{}
	sb.WriteString(`SELECT id, type, severity, resource_id, message, data, read_at, created_at FROM events WHERE 1=1`)
	if filter.Type != "" {
		if !allowedTypes[filter.Type] {
			return nil, store.Validationf("unknown event type %q", filter.Type)
		}
		sb.WriteString(` AND type = ?`)
		args = append(args, filter.Type)
	}
	if filter.Severity != "" {
		if !allowedSeverities[filter.Severity] {
			return nil, store.Validationf("unknown severity %q", filter.Severity)
		}
		sb.WriteString(` AND severity = ?`)
		args = append(args, filter.Severity)
	}
	if filter.Unread != nil {
		if *filter.Unread {
			sb.WriteString(` AND read_at IS NULL`)
		} else {
			sb.WriteString(` AND read_at IS NOT NULL`)
		}
	}
	sb.WriteString(` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`)
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("list events simple: %w", err)
	}
	defer rows.Close()
	var out []domain.Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ev)
	}
	return out, rows.Err()
}

// UnreadCount returns the count of unread events.
func (s *Service) UnreadCount(ctx context.Context, filter ListFilter) (int, error) {
	var sb strings.Builder
	args := []any{}
	sb.WriteString(`SELECT COUNT(*) FROM events WHERE read_at IS NULL`)
	if filter.Type != "" {
		if !allowedTypes[filter.Type] {
			return 0, store.Validationf("unknown event type %q", filter.Type)
		}
		sb.WriteString(` AND type = ?`)
		args = append(args, filter.Type)
	}
	if filter.Severity != "" {
		sb.WriteString(` AND severity = ?`)
		args = append(args, filter.Severity)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, sb.String(), args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("unread count: %w", err)
	}
	return n, nil
}

// MarkRead marks a single event as read.
func (s *Service) MarkRead(ctx context.Context, id domain.ID) (*domain.Event, error) {
	if strings.TrimSpace(string(id)) == "" {
		return nil, store.Validation("event id is required")
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx, `UPDATE events SET read_at = COALESCE(read_at, ?) WHERE id = ?`, now, string(id))
	if err != nil {
		return nil, fmt.Errorf("mark read: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, store.NotFoundf("event %q not found", id)
	}
	return s.Get(ctx, id)
}

// MarkAllRead acknowledges all unread events.
func (s *Service) MarkAllRead(ctx context.Context) error {
	now := s.now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `UPDATE events SET read_at = COALESCE(read_at, ?) WHERE read_at IS NULL`, now)
	if err != nil {
		return fmt.Errorf("mark all read: %w", err)
	}
	return nil
}

// Subscribe returns a channel that receives durable catch-up from since (exclusive) followed by live events.
// The channel is buffered and never blocks the producer; slow subscribers drop events.
func (s *Service) Subscribe(ctx context.Context, since domain.ID) (<-chan domain.Event, func()) {
	ch := make(chan domain.Event, subscriberBuf)
	go func() {
		if since != "" {
			cursor := string(since)
			for {
				if ctx.Err() != nil {
					break
				}
				evs, next, err := s.List(ctx, ListOptions{Cursor: cursor, Limit: 100})
				if err != nil || len(evs) == 0 {
					break
				}
				for _, ev := range evs {
					select {
					case ch <- ev:
					case <-ctx.Done():
						break
					default:
						select {
						case ch <- ev:
						default:
						}
					}
				}
				if next == "" || next == cursor || len(evs) < 100 {
					break
				}
				cursor = next
				if len(evs) == 0 {
					break
				}
			}
		}
		s.mu.Lock()
		if len(s.subs) >= maxSubscribers {
			for k := range s.subs {
				close(s.subs[k])
				delete(s.subs, k)
				break
			}
		}
		id := s.next
		s.next++
		s.subs[id] = ch
		s.mu.Unlock()
		<-ctx.Done()
		s.mu.Lock()
		if c, ok := s.subs[id]; ok && c == ch {
			delete(s.subs, id)
			close(ch)
		}
		s.mu.Unlock()
	}()
	cancel := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for k, c := range s.subs {
			if c == ch {
				delete(s.subs, k)
				close(ch)
				break
			}
		}
	}
	return ch, cancel
}

// Stream forwards Subscribe to a bulk channel, blocking until ctx cancelled.
func (s *Service) Stream(ctx context.Context, since domain.ID, out chan<- domain.Event) error {
	ch, cancel := s.Subscribe(ctx, since)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

func (s *Service) broadcast(ev domain.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (s *Service) getByIdempotency(ctx context.Context, key string) (*domain.Event, error) {
	if key == "" {
		return nil, sql.ErrNoRows
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT id, type, severity, resource_id, message, data, read_at, created_at FROM events WHERE idempotency_key = ?`, key)
	ev, err := scanEvent(row)
	if err != nil {
		if isNotFound(err) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return ev, nil
}

type eventScanner interface{ Scan(dest ...any) error }

func scanEvent(row eventScanner) (*domain.Event, error) {
	var id, typ, sev, rid, msg, dataStr string
	var readAtSQL sql.NullString
	var createdAtStr string
	if err := row.Scan(&id, &typ, &sev, &rid, &msg, &dataStr, &readAtSQL, &createdAtStr); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.NotFound("event not found")
		}
		return nil, err
	}
	var data map[string]any
	if strings.TrimSpace(dataStr) != "" && dataStr != "{}" {
		_ = json.Unmarshal([]byte(dataStr), &data)
		if data != nil {
			data = redactData(data)
		}
	}
	var readAt *time.Time
	if readAtSQL.Valid && strings.TrimSpace(readAtSQL.String) != "" {
		t := parseTime(readAtSQL.String)
		readAt = &t
	}
	created := parseTime(createdAtStr)
	return &domain.Event{
		ID:         domain.ID(id),
		Type:       typ,
		Severity:   sev,
		ResourceID: domain.ID(rid),
		Message:    msg,
		Data:       data,
		ReadAt:     readAt,
		CreatedAt:  created,
	}, nil
}

func parseTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC()
	}
	t, _ := time.Parse("2006-01-02 15:04:05", s)
	return t.UTC()
}

func nullableString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{Valid: false}
	}
	return sql.NullString{String: s, Valid: true}
}

func isNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound) || strings.Contains(strings.ToLower(err.Error()), "not found")
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "constraint") || strings.Contains(msg, "duplicate")
}

func redactData(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		low := strings.ToLower(k)
		redacted := false
		for _, sens := range sensitiveKeys {
			if strings.Contains(low, sens) {
				redacted = true
				break
			}
		}
		if redacted {
			out[k] = "[REDACTED]"
			continue
		}
		switch vv := v.(type) {
		case map[string]any:
			out[k] = redactData(vv)
		case string:
			if len(vv) > 500 {
				out[k] = vv[:500] + "...[TRUNCATED]"
			} else {
				out[k] = vv
			}
		default:
			out[k] = v
		}
	}
	return out
}

func truncateData(data map[string]any, limit int) map[string]any {
	if data == nil {
		return nil
	}
	type kv struct {
		k string
		s int
	}
	var kvs []kv
	for k, v := range data {
		b, _ := json.Marshal(v)
		kvs = append(kvs, kv{k, len(b)})
	}
	sort.Slice(kvs, func(i, j int) bool { return kvs[i].s > kvs[j].s })
	b, _ := json.Marshal(data)
	for len(b) > limit && len(kvs) > 0 {
		delete(data, kvs[0].k)
		kvs = kvs[1:]
		b, _ = json.Marshal(data)
	}
	if len(b) > limit {
		return map[string]any{"_truncated": "[DATA TRUNCATED]"}
	}
	return data
}
