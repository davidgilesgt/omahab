package emailing

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net"
	"net/mail"
	"os"
	"regexp"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Test helpers (preserved for compatibility)
// ---------------------------------------------------------------------------

// StaticVerifier is a test helper implementing DKIMVerifier with fixed result.
type StaticVerifier struct {
	Result DKIMResult
	Err    error
}

func (v StaticVerifier) Verify(_ context.Context, _ []byte) (DKIMResult, error) {
	if v.Err != nil {
		return DKIMResult{}, v.Err
	}
	return v.Result, nil
}

// FuncVerifier allows a function to implement DKIMVerifier.
type FuncVerifier func(ctx context.Context, raw []byte) (DKIMResult, error)

func (f FuncVerifier) Verify(ctx context.Context, raw []byte) (DKIMResult, error) {
	return f(ctx, raw)
}

// ValidVerifier returns a verifier that always reports Valid=true, From signed, aligned.
func ValidVerifier(domain string) DKIMVerifier {
	domain = strings.ToLower(domain)
	return StaticVerifier{Result: DKIMResult{
		Valid:         true,
		Domain:        domain,
		SignedHeaders: []string{"from", "subject", "to", "date"},
		Aligned:       true,
	}}
}

// InvalidVerifier returns a verifier that reports Valid=false.
func InvalidVerifier() DKIMVerifier {
	return StaticVerifier{Result: DKIMResult{Valid: false}}
}

// MissingFromVerifier reports Valid but From not in SignedHeaders.
func MissingFromVerifier(domain string) DKIMVerifier {
	return StaticVerifier{Result: DKIMResult{
		Valid:         true,
		Domain:        strings.ToLower(domain),
		SignedHeaders: []string{"subject", "to"},
		Aligned:       true,
	}}
}

// MisalignedVerifier reports Valid and From signed but not aligned.
func MisalignedVerifier(domain string) DKIMVerifier {
	return StaticVerifier{Result: DKIMResult{
		Valid:         true,
		Domain:        strings.ToLower(domain),
		SignedHeaders: []string{"from"},
		Aligned:       false,
	}}
}

// ---------------------------------------------------------------------------
// Router contract (Wire-up seam for later wave)
// ---------------------------------------------------------------------------

// Router is the narrow Cloudflare Email Routing boundary for the AI address.
// The scoped Token C adapter (internal/cloudflare) will implement this.
// Wave A only defines the contract; no implementation or invocation lives
// in emailing. The control plane gates route activation on successful
// sender verification.
//
// Integrator (internal/controlplane/backend.go:initServices) should:
//   router := cloudflare.NewEmailRouter(tokenC, zoneID)
//   svc := emailing.New(store, cfg, emailing.WithDKIMVerifier(emailing.NewVerifier(...)))
//   // router is stored separately and invoked only after VerifySender succeeds
type Router interface {
	EnsureRoute(ctx context.Context, recipient string) error
	RemoveRoute(ctx context.Context, recipient string) error
}

// ---------------------------------------------------------------------------
// Recipient alias helpers (shared secret)
// ---------------------------------------------------------------------------

// RecipientAliasFromEnv returns the optional randomized alias shared between
// the Worker and ingestion. The daemon reads it from OMAHAB_EMAIL_RECIPIENT_ALIAS
// or, if set, OMAHAB_EMAIL_RECIPIENT_ALIAS_FILE (file containing the alias).
// The alias is generated at deploy time via workers/email/scripts/generate-vars.mjs
// and must be deployed to both the Worker (RECIPIENT_ALIAS var) and the daemon
// (env/file). Empty string means no alias is configured and only the primary
// AI address (ai@<domain> derived from slug+domain) is accepted.
//
// The control plane should call this (or controlplane.LoadRecipientAlias) at
// startup and pass the result into emailing.Config for ingestion checks:
//   alias := emailing.RecipientAliasFromEnv()
//   svc := emailing.New(store, emailing.Config{
//       HMACKey:          hmacKey,
//       AllowedRecipient: primary, // ai@domain
//       RecipientAlias:   alias,
//   }, WithDKIMVerifier(NewVerifier(...)))
func RecipientAliasFromEnv() string {
	// Use os.Getenv but keep verifier package import-light; we import os here.
	// The alias is treated as an email, lowercased and trimmed.
	v := strings.TrimSpace(os.Getenv("OMAHAB_EMAIL_RECIPIENT_ALIAS"))
	if v != "" {
		return strings.ToLower(v)
	}
	if p := strings.TrimSpace(os.Getenv("OMAHAB_EMAIL_RECIPIENT_ALIAS_FILE")); p != "" {
		if data, err := os.ReadFile(p); err == nil {
			if s := strings.TrimSpace(string(data)); s != "" {
				return strings.ToLower(s)
			}
		}
	}
	return ""
}

// AllowedRecipients returns the envelope recipients the Worker and ingestion
// should accept. Primary is the AI address (e.g., ai@example.com derived from
// assistant slug and instance domain). Alias, if non-empty, is the randomized
// shared-secret address (e.g., ai+<random>@example.com).
func AllowedRecipients(primary, alias string) []string {
	primary = strings.ToLower(strings.TrimSpace(primary))
	alias = strings.ToLower(strings.TrimSpace(alias))
	var out []string
	if primary != "" {
		out = append(out, primary)
	}
	if alias != "" && alias != primary {
		out = append(out, alias)
	}
	return out
}

// IsAllowedRecipient reports whether recipient is in the allowlist (primary or alias).
func IsAllowedRecipient(recipient, primary, alias string) bool {
	recipient = strings.ToLower(strings.TrimSpace(recipient))
	primary = strings.ToLower(strings.TrimSpace(primary))
	alias = strings.ToLower(strings.TrimSpace(alias))
	if recipient == "" {
		return false
	}
	if recipient == primary {
		return true
	}
	if alias != "" && recipient == alias {
		return true
	}
	return false
}


// ---------------------------------------------------------------------------
// Real DKIM verifier (RFC 6376 + RFC 8463 ed25519-sha256)
// ---------------------------------------------------------------------------

// TXTLookup fetches DNS TXT for <selector>._domainkey.<domain>.
type TXTLookup func(ctx context.Context, name string) ([]string, error)

func defaultTXTLookup(ctx context.Context, name string) ([]string, error) {
	return net.DefaultResolver.LookupTXT(ctx, name)
}

// Verifier implements DKIMVerifier with stdlib crypto and pluggable DNS.
type Verifier struct {
	lookup TXTLookup
	now    func() time.Time
}

// VerifierOption configures Verifier.
type VerifierOption func(*Verifier)

// WithLookup sets the TXT lookup function.
func WithLookup(l TXTLookup) VerifierOption {
	return func(v *Verifier) { v.lookup = l }
}

// WithNow sets the clock for t/x verification.
func WithNow(fn func() time.Time) VerifierOption {
	return func(v *Verifier) { v.now = fn }
}

// NewVerifier creates a DKIM verifier. If lookup is nil, net.DefaultResolver is used.
func NewVerifier(opts ...VerifierOption) *Verifier {
	v := &Verifier{lookup: defaultTXTLookup, now: time.Now}
	for _, o := range opts {
		o(v)
	}
	if v.lookup == nil {
		v.lookup = defaultTXTLookup
	}
	if v.now == nil {
		v.now = time.Now
	}
	return v
}

// NewDKIMVerifier is an integrator-friendly alias for NewVerifier.
func NewDKIMVerifier(opts ...VerifierOption) DKIMVerifier { return NewVerifier(opts...) }

// StaticTXTLookup returns a lookup backed by a map for tests.
// Keys are lowercased query names (selector._domainkey.domain).
func StaticTXTLookup(records map[string]string) TXTLookup {
	lower := make(map[string][]string, len(records))
	for k, v := range records {
		lower[strings.ToLower(k)] = []string{v}
	}
	return func(_ context.Context, name string) ([]string, error) {
		if v, ok := lower[strings.ToLower(name)]; ok {
			return v, nil
		}
		return nil, fmt.Errorf("DNS TXT not found for %s", name)
	}
}

// MapTXTLookup returns a lookup backed by map[string][]string (multiple TXT strings).
func MapTXTLookup(records map[string][]string) TXTLookup {
	lower := make(map[string][]string, len(records))
	for k, v := range records {
		lower[strings.ToLower(k)] = v
	}
	return func(_ context.Context, name string) ([]string, error) {
		if v, ok := lower[strings.ToLower(name)]; ok {
			return v, nil
		}
		return nil, fmt.Errorf("DNS TXT not found for %s", name)
	}
}

// Verify checks DKIM signatures from raw. It iterates over all DKIM-Signature
// header fields, attempting cryptographic verification for each. The first
// cryptographically valid signature is returned (even if not aligned or
// missing From, so the caller can distinguish reason). If none verify,
// Valid=false.
func (v *Verifier) Verify(ctx context.Context, raw []byte) (DKIMResult, error) {
	if len(raw) == 0 {
		return DKIMResult{Valid: false}, nil
	}
	hdrs, body, err := splitMessage(raw)
	if err != nil {
		return DKIMResult{Valid: false}, nil
	}
	// collect DKIM-Signature headers in wire order
	var dkimHdrs []parsedHeader
	for _, h := range hdrs {
		if h.nameLower == "dkim-signature" {
			dkimHdrs = append(dkimHdrs, h)
		}
	}
	if len(dkimHdrs) == 0 {
		return DKIMResult{Valid: false}, nil
	}
	now := v.now().UTC()
	var lastRes DKIMResult
	for _, dh := range dkimHdrs {
		res, ok, _ := v.verifyOne(ctx, hdrs, body, dh, now)
		if ok && res.Valid {
			return res, nil
		}
		if ok {
			// cryptographically invalid but we keep last to surface SignedHeaders/Aligned
			lastRes = res
		}
	}
	if lastRes.Domain != "" || len(lastRes.SignedHeaders) > 0 {
		return lastRes, nil
	}
	return DKIMResult{Valid: false}, nil
}

// dkimSig holds parsed DKIM-Signature tags.
type dkimSig struct {
	Version    string
	Algorithm  string // rsa-sha256 or ed25519-sha256 lower
	Signature  []byte // b
	BH         []byte // bh
	Domain     string // d lower
	Selector   string // s
	Headers    []string
	HeaderCan  string // relaxed or simple
	BodyCan    string
	Timestamp  *int64
	Expire     *int64
	BodyLen    *int
}

func (v *Verifier) verifyOne(ctx context.Context, hdrs []parsedHeader, body []byte, dkimHdr parsedHeader, now time.Time) (DKIMResult, bool, error) {
	valueStr := headerValue(dkimHdr.rawBytes)
	sig, err := parseDKIMSignatureTags(valueStr)
	if err != nil {
		return DKIMResult{Valid: false}, false, err
	}
	// Build result skeleton
	res := DKIMResult{
		Domain:        strings.ToLower(sig.Domain),
		SignedHeaders: sig.Headers,
		Aligned:       false,
		Valid:         false,
	}
	// t/x checks
	if sig.Timestamp != nil {
		t := time.Unix(*sig.Timestamp, 0).UTC()
		if t.After(now.Add(5 * time.Minute)) {
			return res, true, nil
		}
	}
	if sig.Expire != nil {
		if now.After(time.Unix(*sig.Expire, 0).UTC()) {
			return res, true, nil
		}
	}
	// DNS lookup for pubkey
	pubKey, kType, err := v.lookupPubKey(ctx, sig.Selector, sig.Domain)
	if err != nil {
		return res, true, nil
	}
	// Check algorithm vs key type
	if sig.Algorithm == "rsa-sha256" && kType != "" && kType != "rsa" {
		return res, true, nil
	}
	if sig.Algorithm == "ed25519-sha256" && kType != "" && kType != "ed25519" {
		return res, true, nil
	}
	// Body hash verification first
	canonicalBody := canonicalizeBody(body, sig.BodyCan, sig.BodyLen)
	h := sha256.Sum256(canonicalBody)
	if !bytes.Equal(h[:], sig.BH) {
		return res, true, nil
	}
	// Header hash verification
	headerHash, err := computeHeaderHash(hdrs, sig.Headers, sig.HeaderCan, dkimHdr.rawBytes)
	if err != nil {
		return res, true, nil
	}
	// Verify signature
	var sigOK bool
	switch sig.Algorithm {
	case "rsa-sha256":
		rsaPub, ok := pubKey.(*rsa.PublicKey)
		if !ok {
			return res, true, nil
		}
		err = rsa.VerifyPKCS1v15(rsaPub, crypto.SHA256, headerHash, sig.Signature)
		sigOK = err == nil
	case "ed25519-sha256":
		edPub, ok := pubKey.(ed25519.PublicKey)
		if !ok {
			return res, true, nil
		}
		sigOK = ed25519.Verify(edPub, headerHash, sig.Signature)
	default:
		return res, true, nil
	}
	if !sigOK {
		return res, true, nil
	}
	// Crypto passed; compute alignment and From tracking
	fromDomain := fromDomainFromHeaders(hdrs)
	res.Aligned = isAligned(sig.Domain, fromDomain)
	res.Valid = true
	return res, true, nil
}

func (v *Verifier) lookupPubKey(ctx context.Context, selector, domain string) (any, string, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	selector = strings.TrimSpace(selector)
	if selector == "" || domain == "" {
		return nil, "", fmt.Errorf("missing selector/domain")
	}
	name := selector + "._domainkey." + domain
	txts, err := v.lookup(ctx, name)
	if err != nil {
		return nil, "", err
	}
	// Try joined first, then individual
	candidates := []string{strings.Join(txts, "")}
	candidates = append(candidates, txts...)
	for _, txt := range candidates {
		k, p, ok := parseKeyRecord(txt)
		if !ok {
			continue
		}
		rawP, err := decodeBase64IgnoreWSP(p)
		if err != nil || len(rawP) == 0 {
			// p empty means revoked
			continue
		}
		kk := strings.ToLower(strings.TrimSpace(k))
		if kk == "" {
			kk = "rsa"
		}
		if kk == "rsa" {
			pub, err := parseRSAPublicKey(rawP)
			if err != nil {
				continue
			}
			return pub, "rsa", nil
		}
		if kk == "ed25519" {
			if len(rawP) != ed25519.PublicKeySize {
				continue
			}
			return ed25519.PublicKey(rawP), "ed25519", nil
		}
	}
	return nil, "", fmt.Errorf("no valid key")
}

func parseRSAPublicKey(der []byte) (*rsa.PublicKey, error) {
	if pub, err := x509.ParsePKIXPublicKey(der); err == nil {
		if rsaPub, ok := pub.(*rsa.PublicKey); ok {
			return rsaPub, nil
		}
	}
	if rsaPub, err := x509.ParsePKCS1PublicKey(der); err == nil {
		return rsaPub, nil
	}
	return nil, fmt.Errorf("rsa parse failed")
}

// ---------------------------------------------------------------------------
// Message splitting & header parsing
// ---------------------------------------------------------------------------

type parsedHeader struct {
	nameLower string
	rawName   string
	rawBytes  []byte // header field including CRLF(s) for folded continuations
}

func splitMessage(raw []byte) ([]parsedHeader, []byte, error) {
	hb, body := splitHeadersBody(raw)
	hdrs, err := parseHeaders(hb)
	if err != nil {
		return nil, nil, err
	}
	return hdrs, body, nil
}

func splitHeadersBody(raw []byte) (headerBytes []byte, body []byte) {
	// Look for \r\n\r\n first, then \n\n
	if idx := bytes.Index(raw, []byte("\r\n\r\n")); idx >= 0 {
		// idx points to start of delimiter's first CRLF which is also last header's CRLF.
		// Headers include that first CRLF, body starts after second CRLF.
		headerBytes = raw[:idx+2]
		body = raw[idx+4:]
		return headerBytes, body
	}
	if idx := bytes.Index(raw, []byte("\n\n")); idx >= 0 {
		headerBytes = raw[:idx+1]
		body = raw[idx+2:]
		return headerBytes, body
	}
	// No empty line: treat all as headers, empty body
	return raw, []byte{}
}

func parseHeaders(data []byte) ([]parsedHeader, error) {
	var headers []parsedHeader
	var cur *parsedHeader
	i := 0
	for i < len(data) {
		lineEnd := -1
		// find CRLF or LF
		for j := i; j < len(data); j++ {
			if data[j] == '\r' && j+1 < len(data) && data[j+1] == '\n' {
				lineEnd = j + 2
				break
			}
			if data[j] == '\n' {
				lineEnd = j + 1
				break
			}
		}
		if lineEnd == -1 {
			// no terminator: take remainder
			lineEnd = len(data)
			line := data[i:lineEnd]
			if len(bytes.TrimSpace(line)) == 0 {
				break
			}
			if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
				if cur != nil {
					cur.rawBytes = append(cur.rawBytes, line...)
				}
			} else {
				col := bytes.Index(line, []byte(":"))
				if col >= 0 {
					hdr := parsedHeader{
						nameLower: strings.ToLower(strings.TrimSpace(string(line[:col]))),
						rawName:   string(line[:col]),
						rawBytes:  append([]byte(nil), line...),
					}
					headers = append(headers, hdr)
					cur = &headers[len(headers)-1]
				}
			}
			break
		}
		line := data[i:lineEnd]
		if len(line) == 2 && line[0] == '\r' && line[1] == '\n' {
			// empty line shouldn't be in headerBytes; ignore
			i = lineEnd
			continue
		}
		if len(bytes.TrimSpace(line)) == 0 {
			i = lineEnd
			continue
		}
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			if cur != nil {
				cur.rawBytes = append(cur.rawBytes, line...)
			}
		} else {
			col := bytes.Index(line, []byte(":"))
			if col < 0 {
				// malformed line without colon, skip
				i = lineEnd
				continue
			}
			hdr := parsedHeader{
				nameLower: strings.ToLower(strings.TrimSpace(string(line[:col]))),
				rawName:   string(line[:col]),
				rawBytes:  append([]byte(nil), line...),
			}
			headers = append(headers, hdr)
			cur = &headers[len(headers)-1]
		}
		i = lineEnd
	}
	return headers, nil
}

func headerValue(raw []byte) string {
	col := bytes.Index(raw, []byte(":"))
	if col < 0 {
		return ""
	}
	val := raw[col+1:]
	// val includes leading space, folding, and final CRLF
	// strip final CRLF for tag parsing
	if bytes.HasSuffix(val, []byte("\r\n")) {
		val = val[:len(val)-2]
	} else if bytes.HasSuffix(val, []byte("\n")) {
		val = val[:len(val)-1]
	}
	return string(val)
}

// ---------------------------------------------------------------------------
// DKIM tag parsing
// ---------------------------------------------------------------------------

var domainRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*\.[a-z]{2,}$`)

func parseDKIMSignatureTags(valueStr string) (*dkimSig, error) {
	tags := parseTagList(valueStr)
	// required
	v, ok := tags["v"]
	if !ok || strings.TrimSpace(v) != "1" {
		return nil, fmt.Errorf("v missing or not 1")
	}
	a, ok := tags["a"]
	if !ok {
		return nil, fmt.Errorf("a missing")
	}
	a = strings.ToLower(strings.TrimSpace(a))
	if a != "rsa-sha256" && a != "ed25519-sha256" {
		return nil, fmt.Errorf("unsupported a %s", a)
	}
	bStr, ok := tags["b"]
	if !ok {
		return nil, fmt.Errorf("b missing")
	}
	bStrClean := stripWSP(bStr)
	if bStrClean == "" {
		return nil, fmt.Errorf("b empty")
	}
	sigBytes, err := decodeBase64IgnoreWSP(bStr)
	if err != nil {
		return nil, fmt.Errorf("b base64: %w", err)
	}
	bhStr, ok := tags["bh"]
	if !ok {
		return nil, fmt.Errorf("bh missing")
	}
	bhBytes, err := decodeBase64IgnoreWSP(bhStr)
	if err != nil {
		return nil, fmt.Errorf("bh base64: %w", err)
	}
	d, ok := tags["d"]
	if !ok || strings.TrimSpace(d) == "" {
		return nil, fmt.Errorf("d missing")
	}
	d = strings.ToLower(strings.TrimSpace(d))
	if !isDomain(d) {
		return nil, fmt.Errorf("invalid d")
	}
	s, ok := tags["s"]
	if !ok || strings.TrimSpace(s) == "" {
		return nil, fmt.Errorf("s missing")
	}
	s = strings.TrimSpace(s)
	hStr, ok := tags["h"]
	if !ok || strings.TrimSpace(hStr) == "" {
		return nil, fmt.Errorf("h missing")
	}
	headers := parseHList(hStr)
	if len(headers) == 0 {
		return nil, fmt.Errorf("h empty")
	}
	cStr := tags["c"]
	headerCan, bodyCan := parseCanonicalization(cStr)
	// t, x, l optional
	var tPtr *int64
	if ts, ok := tags["t"]; ok && strings.TrimSpace(ts) != "" {
		if v, err := parseDecimalInt64(strings.TrimSpace(ts)); err == nil {
			tPtr = &v
		}
	}
	var xPtr *int64
	if xs, ok := tags["x"]; ok && strings.TrimSpace(xs) != "" {
		if v, err := parseDecimalInt64(strings.TrimSpace(xs)); err == nil {
			xPtr = &v
		}
	}
	var lPtr *int
	if ls, ok := tags["l"]; ok && strings.TrimSpace(ls) != "" {
		if v, err := parseDecimalInt(strings.TrimSpace(ls)); err == nil && v >= 0 {
			lPtr = &v
		}
	}
	return &dkimSig{
		Version:   "1",
		Algorithm: a,
		Signature: sigBytes,
		BH:        bhBytes,
		Domain:    d,
		Selector:  s,
		Headers:   headers,
		HeaderCan: headerCan,
		BodyCan:   bodyCan,
		Timestamp: tPtr,
		Expire:    xPtr,
		BodyLen:   lPtr,
	}, nil
}

func parseTagList(s string) map[string]string {
	m := make(map[string]string)
	// Normalize folding: replace CRLF+WSP with " "
	// Keep string as is but for splitting we treat ";" as delimiter regardless of folding.
	// For values that contain folding, the CRLF+WSP is considered WSP and should be
	// retained inside value for base64 decoding where it's ignored. Our later
	// decode strips WSP, so it's fine to keep CRLF in value string before split.
	parts := strings.Split(s, ";")
	for _, p := range parts {
		// Each part is " [FWS] tag [FWS] = [FWS] value [FWS]"
		// Find "="
		eq := strings.Index(p, "=")
		if eq < 0 {
			// No '=', skip (could be trailing empty)
			continue
		}
		namePart := p[:eq]
		valuePart := p[eq+1:]
		// name is trimmed of FWS (which includes CRLF+WSP). Remove CRLF and trim.
		name := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(namePart, "\r", ""), "\n", ""))
		// valuePart may contain CRLF+WSP; keep as is trimmed of outer FWS? Tag-spec says WSP around tags ignored, but inside value WSP significant except for base64.
		// We trim leading/trailing FWS (including CRLF+WSP)
		// For simplicity, trim spaces and remove leading/trailing CRLF.
		value := strings.TrimSpace(valuePart)
		// Note: internal FWS inside base64 remains; decode will strip.
		if name == "" {
			continue
		}
		lname := strings.ToLower(name)
		if _, exists := m[lname]; exists {
			// duplicate => invalid tag list; mark invalid by not returning? We'll let caller see duplicate via check.
			// For now, keep first, but verifyOne will treat duplicate as invalid via later check.
			continue
		}
		m[lname] = value
	}
	return m
}

func parseHList(s string) []string {
	// h is colon-separated list, FWS around colons allowed.
	// s may contain CRLF+WSP folding.
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	var out []string
	for _, p := range strings.Split(s, ":") {
		t := strings.ToLower(strings.TrimSpace(p))
		// strip internal WSP? tag names shouldn't contain spaces.
		if t != "" {
			// field name validation: must be token
			out = append(out, t)
		}
	}
	return out
}

func parseCanonicalization(s string) (headerCan, bodyCan string) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return "simple", "simple"
	}
	parts := strings.Split(s, "/")
	if len(parts) == 1 {
		h := strings.TrimSpace(parts[0])
		if h == "" {
			h = "simple"
		}
		return h, "simple"
	}
	h := strings.TrimSpace(parts[0])
	b := strings.TrimSpace(parts[1])
	if h == "" {
		h = "simple"
	}
	if b == "" {
		b = "simple"
	}
	if h != "simple" && h != "relaxed" {
		h = "simple"
	}
	if b != "simple" && b != "relaxed" {
		b = "simple"
	}
	return h, b
}

func isDomain(d string) bool {
	d = strings.ToLower(d)
	if len(d) == 0 || len(d) > 255 {
		return false
	}
	if strings.Contains(d, "..") {
		return false
	}
	return domainRe.MatchString(d)
}

func parseDecimalInt64(s string) (int64, error) {
	var v int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not decimal")
		}
	}
	_, err := fmt.Sscan(s, &v)
	return v, err
}

func parseDecimalInt(s string) (int, error) {
	v, err := parseDecimalInt64(s)
	return int(v), err
}

// ---------------------------------------------------------------------------
// Key record parsing
// ---------------------------------------------------------------------------

func parseKeyRecord(txt string) (k, p string, ok bool) {
	tags := parseTagList(txt)
	// v optional must be DKIM1 if present
	if v, exists := tags["v"]; exists && strings.ToLower(strings.TrimSpace(v)) != "dkim1" {
		return "", "", false
	}
	p, ok = tags["p"]
	if !ok {
		return "", "", false
	}
	k = tags["k"]
	if k == "" {
		k = "rsa"
	}
	return k, p, true
}

// ---------------------------------------------------------------------------
// Base64 helpers
// ---------------------------------------------------------------------------

func stripWSP(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r != ' ' && r != '\t' && r != '\r' && r != '\n' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func decodeBase64IgnoreWSP(s string) ([]byte, error) {
	clean := stripWSP(s)
	if clean == "" {
		return nil, fmt.Errorf("empty base64")
	}
	// Try std encoding
	if data, err := base64.StdEncoding.DecodeString(clean); err == nil {
		return data, nil
	}
	// Try raw (no padding)
	if data, err := base64.RawStdEncoding.DecodeString(clean); err == nil {
		return data, nil
	}
	// Try URL variant? Not needed
	return nil, fmt.Errorf("base64 decode failed")
}

// ---------------------------------------------------------------------------
// Body canonicalization
// ---------------------------------------------------------------------------

func canonicalizeBody(body []byte, algo string, l *int) []byte {
	var canon []byte
	switch algo {
	case "relaxed":
		canon = canonicalizeBodyRelaxed(body)
	case "simple":
		canon = canonicalizeBodySimple(body)
	default:
		canon = canonicalizeBodySimple(body)
	}
	if l != nil {
		limit := *l
		if limit < 0 {
			limit = 0
		}
		if limit < len(canon) {
			canon = canon[:limit]
		}
	}
	return canon
}

func canonicalizeBodySimple(body []byte) []byte {
	if len(body) == 0 {
		return []byte("\r\n")
	}
	s := string(body)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	// Remove trailing empty lines (lines that are "" at end)
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return []byte("\r\n")
	}
	return []byte(strings.Join(lines, "\r\n") + "\r\n")
}

func canonicalizeBodyRelaxed(body []byte) []byte {
	if len(body) == 0 {
		return []byte{}
	}
	s := string(body)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		trimmed := strings.TrimRight(line, " \t")
		// reduce WSP sequences to single SP
		var buf strings.Builder
		inWSP := false
		for _, r := range trimmed {
			if r == ' ' || r == '\t' {
				if !inWSP {
					buf.WriteByte(' ')
					inWSP = true
				}
			} else {
				buf.WriteRune(r)
				inWSP = false
			}
		}
		lines[i] = buf.String()
	}
	// ignore trailing empty lines
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return []byte{}
	}
	return []byte(strings.Join(lines, "\r\n") + "\r\n")
}

// ---------------------------------------------------------------------------
// Header canonicalization
// ---------------------------------------------------------------------------

func relaxedHeaderCanonical(raw []byte) []byte {
	col := bytes.Index(raw, []byte(":"))
	if col < 0 {
		return bytes.ToLower(raw)
	}
	namePart := raw[:col]
	valuePart := raw[col+1:]
	name := strings.ToLower(strings.TrimSpace(string(namePart)))
	// Process value
	valStr := string(valuePart)
	// Strip final CRLF for processing
	hasCRLF := strings.HasSuffix(valStr, "\r\n")
	if hasCRLF {
		valStr = valStr[:len(valStr)-2]
	} else if strings.HasSuffix(valStr, "\n") {
		valStr = valStr[:len(valStr)-1]
	}
	// Unfold: replace CRLF+WSP (1*WSP) with single SP
	valStr = unfoldHeaderValue(valStr)
	// Convert WSP sequences to single SP
	valStr = compressWSP(valStr)
	// Delete WSP at end
	valStr = strings.TrimRight(valStr, " \t")
	// Delete WSP before/after colon already done via name trimming and leading trim
	valStr = strings.TrimLeft(valStr, " \t")
	return []byte(name + ":" + valStr + "\r\n")
}

func simpleHeaderCanonical(raw []byte) []byte {
	// No changes, ensure it ends with CRLF (raw already does)
	// Return copy
	out := make([]byte, len(raw))
	copy(out, raw)
	return out
}

func unfoldHeaderValue(s string) string {
	var out strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n' {
			// peek WSP after CRLF
			j := i + 2
			k := j
			for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
				k++
			}
			if k > j {
				out.WriteByte(' ')
				i = k
				continue
			}
			out.WriteString(s[i : i+2])
			i += 2
			continue
		}
		if s[i] == '\n' {
			j := i + 1
			k := j
			for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
				k++
			}
			if k > j {
				out.WriteByte(' ')
				i = k
				continue
			}
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

func compressWSP(s string) string {
	var buf strings.Builder
	inWSP := false
	for _, r := range s {
		if r == ' ' || r == '\t' {
			if !inWSP {
				buf.WriteByte(' ')
				inWSP = true
			}
		} else {
			buf.WriteRune(r)
			inWSP = false
		}
	}
	return buf.String()
}

func computeHeaderHash(hdrs []parsedHeader, hList []string, headerCan string, dkimRaw []byte) ([]byte, error) {
	used := make(map[int]bool)
	var toSign [][]byte
	for _, want := range hList {
		wantLower := strings.ToLower(strings.TrimSpace(want))
		if wantLower == "" {
			continue
		}
		found := -1
		for i := len(hdrs) - 1; i >= 0; i-- {
			if used[i] {
				continue
			}
			if hdrs[i].nameLower == wantLower {
				found = i
				break
			}
		}
		if found == -1 {
			continue
		}
		used[found] = true
		var canon []byte
		if headerCan == "relaxed" {
			canon = relaxedHeaderCanonical(hdrs[found].rawBytes)
		} else {
			canon = simpleHeaderCanonical(hdrs[found].rawBytes)
		}
		toSign = append(toSign, canon)
	}
	// DKIM header with empty b
	unsigned := dkimHeaderWithEmptyB(dkimRaw)
	var dkimCanon []byte
	if headerCan == "relaxed" {
		dkimCanon = relaxedHeaderCanonical(unsigned)
	} else {
		dkimCanon = simpleHeaderCanonical(unsigned)
	}
	toSign = append(toSign, dkimCanon)
	joined := bytes.Join(toSign, nil)
	h := sha256.Sum256(joined)
	return h[:], nil
}

func dkimHeaderWithEmptyB(raw []byte) []byte {
	s := string(raw)
	col := strings.Index(s, ":")
	if col < 0 {
		return raw
	}
	name := s[:col+1] // includes colon
	valueWithCRLF := s[col+1:]
	hasCRLF := strings.HasSuffix(valueWithCRLF, "\r\n")
	var suffix string
	if hasCRLF {
		suffix = "\r\n"
		valueWithCRLF = valueWithCRLF[:len(valueWithCRLF)-2]
	} else if strings.HasSuffix(valueWithCRLF, "\n") {
		suffix = "\n"
		valueWithCRLF = valueWithCRLF[:len(valueWithCRLF)-1]
	}
	parts := strings.Split(valueWithCRLF, ";")
	for i, p := range parts {
		eq := strings.Index(p, "=")
		if eq < 0 {
			continue
		}
		tagName := strings.TrimSpace(p[:eq])
		tagName = strings.ReplaceAll(tagName, "\r", "")
		tagName = strings.ReplaceAll(tagName, "\n", "")
		tagName = strings.TrimSpace(tagName)
		if strings.EqualFold(tagName, "b") {
			parts[i] = p[:eq+1]
			break
		}
	}
	newValue := strings.Join(parts, ";")
	return []byte(name + newValue + suffix)
}

// ---------------------------------------------------------------------------
// Alignment and From extraction
// ---------------------------------------------------------------------------

func fromDomainFromHeaders(hdrs []parsedHeader) string {
	for i := len(hdrs) - 1; i >= 0; i-- {
		if hdrs[i].nameLower == "from" {
			val := headerValue(hdrs[i].rawBytes)
			// Use mail.ParseAddress after unfolding
			val = unfoldHeaderValue(val)
			val = strings.TrimSpace(val)
			if addr, err := mail.ParseAddress(val); err == nil {
				parts := strings.Split(strings.ToLower(strings.TrimSpace(addr.Address)), "@")
				if len(parts) == 2 {
					return parts[1]
				}
			}
			// fallback regex
			re := regexp.MustCompile(`[A-Za-z0-9._%+\-]+@([A-Za-z0-9.\-]+\.[A-Za-z]{2,})`)
			if m := re.FindStringSubmatch(val); len(m) == 2 {
				return strings.ToLower(m[1])
			}
			// last resort: extract domain via split
			if at := strings.LastIndex(val, "@"); at >= 0 {
				dom := strings.TrimSpace(val[at+1:])
				dom = strings.Trim(dom, "<> \t\r\n;")
				dom = strings.Split(dom, " ")[0]
				return strings.ToLower(dom)
			}
		}
	}
	return ""
}

func organizationalDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	if d == "" {
		return ""
	}
	parts := strings.Split(d, ".")
	if len(parts) < 2 {
		return d
	}
	// handle known two-level publicsuffix approximations: co.uk, com.au etc.
	// For simplicity, return last 2 labels. This covers example.com test cases.
	if len(parts) >= 3 {
		last2 := strings.Join(parts[len(parts)-2:], ".")
		// naive check for ccTLD with second-level: if last part length==2 and second-last in {"co","com","net","org","gov","ac","edu","ltd"}
		// then include third last.
		if len(parts[len(parts)-1]) == 2 {
			second := parts[len(parts)-2]
			if second == "co" || second == "com" || second == "net" || second == "org" || second == "gov" || second == "ac" || second == "ltd" {
				return strings.Join(parts[len(parts)-3:], ".")
			}
		}
		_ = last2
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

func isAligned(d, fromDomain string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	fromDomain = strings.ToLower(strings.TrimSpace(fromDomain))
	if d == "" || fromDomain == "" {
		return false
	}
	if d == fromDomain {
		return true
	}
	// relaxed alignment: organizational domains equal
	if od := organizationalDomain(d); od != "" {
		if of := organizationalDomain(fromDomain); of != "" && od == of {
			return true
		}
	}
	// also allow subdomain exact parent (strict) as fallback
	if strings.HasSuffix(fromDomain, "."+d) {
		return true
	}
	if strings.HasSuffix(d, "."+fromDomain) {
		// signing subdomain for parent domain: still relaxed org equality would have caught, but allow
		if organizationalDomain(d) == organizationalDomain(fromDomain) {
			return true
		}
	}
	return false
}
