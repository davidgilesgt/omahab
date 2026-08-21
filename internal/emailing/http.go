package emailing

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// ForwardPayload mirrors workers/email ForwardPayload JSON.
type ForwardPayload struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Timestamp string `json:"timestamp"`
	Nonce     string `json:"nonce"`
	Raw       string `json:"raw"` // base64
	RawSize   int    `json:"rawSize"`
}

// WebhookHandler returns an http.Handler for the narrow Worker-to-omahabd
// ingestion endpoint. It expects:
//
//	Headers: x-omahab-timestamp (decimal string), x-omahab-nonce,
//	         x-omahab-signature ("sha256=<hex>"), x-omahab-from, x-omahab-to
//	Body: JSON ForwardPayload with base64 raw and rawSize.
//
// For backwards compatibility it also accepts a legacy raw RFC822 body with
// the same headers (timestamp/nonce/signature) and no JSON.
// The handler verifies HMAC over the v1 canonical
//
//	v1\n<timestamp>\n<nonce>\n<from>\n<to>\n<raw.length>\n<raw bytes>
//
// and delegates to Service.Ingest. It never logs raw content.
func (s *Service) WebhookHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Normalize headers (case-insensitive).
		tsHeader := strings.TrimSpace(r.Header.Get("x-omahab-timestamp"))
		if tsHeader == "" {
			tsHeader = strings.TrimSpace(r.Header.Get("X-Omahab-Timestamp"))
		}
		nonceHeader := strings.TrimSpace(r.Header.Get("x-omahab-nonce"))
		if nonceHeader == "" {
			nonceHeader = strings.TrimSpace(r.Header.Get("X-Omahab-Nonce"))
		}
		sigHeader := strings.TrimSpace(r.Header.Get("x-omahab-signature"))
		if sigHeader == "" {
			sigHeader = strings.TrimSpace(r.Header.Get("X-Omahab-Signature"))
		}
		fromHeader := strings.TrimSpace(r.Header.Get("x-omahab-from"))
		if fromHeader == "" {
			fromHeader = strings.TrimSpace(r.Header.Get("X-Omahab-From"))
		}
		toHeader := strings.TrimSpace(r.Header.Get("x-omahab-to"))
		if toHeader == "" {
			toHeader = strings.TrimSpace(r.Header.Get("X-Omahab-To"))
		}

		if tsHeader == "" || nonceHeader == "" || sigHeader == "" {
			http.Error(w, "missing authentication headers", http.StatusUnauthorized)
			return
		}

		// Read body with limit.
		limit := int64(s.cfg.rawLimit() + 1024 + 16*1024) // allow JSON overhead
		if limit < 32*1024 {
			limit = 32 * 1024
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		if int64(len(body)) > limit {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}

		// Try JSON payload first (worker canonical).
		var payload ForwardPayload
		var raw []byte
		var ts, nonce, from, to, sig string
		var rawSize int

		bodyTrim := bytes.TrimSpace(body)
		if len(bodyTrim) > 0 && bodyTrim[0] == '{' {
			if err := json.Unmarshal(bodyTrim, &payload); err == nil && payload.Raw != "" {
				// JSON path
				ts = strings.TrimSpace(payload.Timestamp)
				nonce = strings.TrimSpace(payload.Nonce)
				from = strings.TrimSpace(payload.From)
				to = strings.TrimSpace(payload.To)
				sig = sigHeader // signature always from header, not payload
				rawSize = payload.RawSize

				// If header values present, they must match payload (prevent header/payload desync).
				if tsHeader != "" && tsHeader != ts {
					http.Error(w, "timestamp mismatch", http.StatusUnauthorized)
					return
				}
				if nonceHeader != "" && nonceHeader != nonce {
					http.Error(w, "nonce mismatch", http.StatusUnauthorized)
					return
				}
				if fromHeader != "" && fromHeader != from {
					http.Error(w, "from mismatch", http.StatusUnauthorized)
					return
				}
				if toHeader != "" && toHeader != to {
					http.Error(w, "to mismatch", http.StatusUnauthorized)
					return
				}
				// Prefer header values when payload empty (worker always sends both).
				if ts == "" {
					ts = tsHeader
				}
				if nonce == "" {
					nonce = nonceHeader
				}
				if from == "" {
					from = fromHeader
				}
				if to == "" {
					to = toHeader
				}

				// Decode base64 raw.
				decoded, err := base64.StdEncoding.DecodeString(payload.Raw)
				if err != nil {
					// Try without padding or url variants
					decoded, err = base64.RawStdEncoding.DecodeString(payload.Raw)
					if err != nil {
						http.Error(w, "invalid base64 raw", http.StatusBadRequest)
						return
					}
				}
				raw = decoded
				if rawSize != 0 && rawSize != len(raw) {
					http.Error(w, "rawSize mismatch", http.StatusBadRequest)
					return
				}
				if len(raw) > s.cfg.rawLimit() {
					http.Error(w, "raw too large", http.StatusRequestEntityTooLarge)
					return
				}
				// Validate timestamp format early for better error.
				if _, err := strconv.ParseInt(ts, 10, 64); err != nil {
					http.Error(w, "invalid timestamp", http.StatusBadRequest)
					return
				}
				res, err := s.Ingest(r.Context(), IngestRequest{
					TimestampStr: ts,
					Nonce:        nonce,
					From:         from,
					To:           to,
					Raw:          raw,
					Signature:    sig,
					RawSize:      len(raw),
				})
				handleIngestResult(w, res, err)
				return
			}
		}

		// Legacy fallback: body is raw RFC822, headers carry auth.
		// This path supports older tests that POST raw bytes directly.
		raw = body
		ts = tsHeader
		nonce = nonceHeader
		from = fromHeader
		to = toHeader
		sig = sigHeader
		if len(raw) > s.cfg.rawLimit() {
			http.Error(w, "raw too large", http.StatusRequestEntityTooLarge)
			return
		}
		// Try to parse timestamp for validation.
		if _, err := strconv.ParseInt(ts, 10, 64); err != nil {
			http.Error(w, "invalid timestamp", http.StatusBadRequest)
			return
		}
		// For legacy, raw may be JSON that failed to parse as ForwardPayload but is still raw; keep as is.
		res, err := s.Ingest(r.Context(), IngestRequest{
			TimestampStr: ts,
			Nonce:        nonce,
			From:         from,
			To:           to,
			Raw:          raw,
			Signature:    sig,
			RawSize:      len(raw),
		})
		// Also try int64 timestamp path for callers that use legacy IngestRequest with int64
		// If Ingest failed with auth, try interpreting ts as int64 for legacy HMAC framing.
		if err == ErrAuthFailed && from == "" && to == "" {
			if tsInt, err2 := strconv.ParseInt(ts, 10, 64); err2 == nil {
				if res2, err3 := s.Ingest(r.Context(), IngestRequest{
					Timestamp: tsInt,
					Nonce:     nonce,
					Raw:       raw,
					Signature: sig,
				}); err3 == nil || err3 == ErrQuarantined {
					handleIngestResult(w, res2, err3)
					return
				}
			}
		}
		handleIngestResult(w, res, err)
	})
}

func handleIngestResult(w http.ResponseWriter, res IngestResult, err error) {
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case isAuthError(err):
			status = http.StatusUnauthorized
		case err == ErrReplay:
			status = http.StatusConflict
		case err == ErrClockSkew:
			status = http.StatusBadRequest
		case err == ErrTooLarge:
			status = http.StatusRequestEntityTooLarge
		case err == ErrQuarantined:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     res.MessageID,
				"status": res.Status,
				"reason": res.Quarantine.Reason,
			})
			return
		}
		http.Error(w, http.StatusText(status), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":     res.MessageID,
		"status": res.Status,
	})
}

func isAuthError(err error) bool {
	return err == ErrAuthFailed || strings.Contains(err.Error(), ErrAuthFailed.Error())
}
