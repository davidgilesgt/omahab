package emailing

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"regexp"
	"strings"
)

// parseMessage parses raw RFC 5322 bytes into a ParsedMessage. It enforces
// decoded size accounting and treats all content as untrusted.
// envelopeFrom and recipient are supplied by the Worker envelope; headerFrom
// comes from the MIME headers. All size checks are performed before returning.
func parseMessage(raw []byte, envelopeFrom, recipient string, decodedLimit int) (*ParsedMessage, error) {
	if len(raw) == 0 {
		return nil, ErrValidation
	}

	// Use net/mail to parse headers. It handles folded headers and requires
	// a valid header block.
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, ErrValidation
	}

	headerFrom := msg.Header.Get("From")
	subject := msg.Header.Get("Subject")
	// Decode RFC 2047 encoded-words in Subject/From for storage, but keep safe.
	subject = decodeWordSafe(subject)

	// Extract addr-spec from From header for exact sender checks.
	fromAddress, fromDomain := extractAddrSpec(headerFrom)

	// Prepare parsed skeleton.
	parsed := &ParsedMessage{
		EnvelopeFrom: envelopeFrom,
		HeaderFrom:   headerFrom,
		FromAddress:  fromAddress,
		FromDomain:   fromDomain,
		Recipient:    recipient,
		Subject:      subject,
		RawSize:      len(raw),
	}

	// Content-Type handling.
	ct := msg.Header.Get("Content-Type")
	if ct == "" {
		ct = "text/plain; charset=utf-8"
	}
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		mediaType = "text/plain"
		params = map[string]string{"charset": "utf-8"}
	}

	decodedSize := 0
	var textBody, htmlBody string
	var attachments []Attachment
	var links []string

	if strings.HasPrefix(mediaType, "multipart/") {
		boundary, ok := params["boundary"]
		if !ok {
			return nil, ErrValidation
		}
		mr := multipart.NewReader(msg.Body, boundary)
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, ErrValidation
			}
			partCT := part.Header.Get("Content-Type")
			if partCT == "" {
				partCT = "text/plain; charset=utf-8"
			}
			pt, pp, _ := mime.ParseMediaType(partCT)
			if pt == "" {
				pt = "text/plain"
			}
			disp, dispParams, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
			filename := ""
			if dispParams != nil {
				filename = dispParams["filename"]
				if filename == "" {
					filename = part.Header.Get("Content-Disposition")
					// fallback: try to extract filename param manually
				}
			}
			// If disposition is attachment or filename present, treat as attachment.
			isAttachment := disp == "attachment" || filename != ""
			// Also treat non-text types as attachment if not explicitly inline text.
			if !isAttachment && pt != "text/plain" && pt != "text/html" && !strings.HasPrefix(pt, "multipart/") {
				// Heuristic: application/*, image/*, etc. are attachments.
				if strings.HasPrefix(pt, "application/") || strings.HasPrefix(pt, "image/") || strings.HasPrefix(pt, "video/") || strings.HasPrefix(pt, "audio/") {
					isAttachment = true
				}
			}
			// Handle nested multipart recursively by reading raw and re-parsing?
			// Simplify: if part is multipart, read its body as nested multipart.
			if strings.HasPrefix(pt, "multipart/") {
				// Read remaining bytes of this part and parse as multipart.
				// We need boundary for nested part.
				nestedBoundary, ok := pp["boundary"]
				if ok {
					bodyBytes, err := io.ReadAll(part)
					if err != nil {
						return nil, ErrTooLarge
					}
					// Enforce decoded limit on nested raw too.
					decodedSize += len(bodyBytes)
					if decodedLimit > 0 && decodedSize > decodedLimit {
						return nil, ErrTooLarge
					}
					nmr := multipart.NewReader(bytes.NewReader(bodyBytes), nestedBoundary)
					for {
						np, err := nmr.NextPart()
						if err == io.EOF {
							break
						}
						if err != nil {
							break
						}
						npCT := np.Header.Get("Content-Type")
						if npCT == "" {
							npCT = "text/plain; charset=utf-8"
						}
						npt, _, _ := mime.ParseMediaType(npCT)
						ndisp, ndp, _ := mime.ParseMediaType(np.Header.Get("Content-Disposition"))
						nfilename := ""
						if ndp != nil {
							nfilename = ndp["filename"]
						}
						nisAttach := ndisp == "attachment" || nfilename != ""
						if !nisAttach && npt != "text/plain" && npt != "text/html" {
							if strings.HasPrefix(npt, "application/") || strings.HasPrefix(npt, "image/") {
								nisAttach = true
							}
						}
						enc := strings.ToLower(np.Header.Get("Content-Transfer-Encoding"))
						rawPart, err := readPartBody(np, enc)
						if err != nil {
							return nil, err
						}
						decodedSize += len(rawPart)
						if decodedLimit > 0 && decodedSize > decodedLimit {
							return nil, ErrTooLarge
						}
						if nisAttach {
							attachments = append(attachments, Attachment{
								Filename:    nfilename,
								ContentType: npt,
								Data:        rawPart,
								Size:        len(rawPart),
							})
						} else {
							if npt == "text/html" {
								htmlBody += string(rawPart)
								links = append(links, extractLinks(string(rawPart))...)
							} else {
								textBody += string(rawPart)
								links = append(links, extractLinks(string(rawPart))...)
							}
						}
					}
					continue
				}
			}

			enc := strings.ToLower(part.Header.Get("Content-Transfer-Encoding"))
			bodyBytes, err := readPartBody(part, enc)
			if err != nil {
				return nil, err
			}
			decodedSize += len(bodyBytes)
			if decodedLimit > 0 && decodedSize > decodedLimit {
				return nil, ErrTooLarge
			}
			if isAttachment {
				attachments = append(attachments, Attachment{
					Filename:    filename,
					ContentType: pt,
					Data:        bodyBytes,
					Size:        len(bodyBytes),
				})
			} else {
				if pt == "text/html" {
					htmlBody += string(bodyBytes)
					links = append(links, extractLinks(string(bodyBytes))...)
				} else {
					// text/plain
					textBody += string(bodyBytes)
					links = append(links, extractLinks(string(bodyBytes))...)
				}
			}
		}
	} else {
		// Single part.
		enc := strings.ToLower(msg.Header.Get("Content-Transfer-Encoding"))
		bodyBytes, err := readPartBody(msg.Body, enc)
		if err != nil {
			return nil, err
		}
		decodedSize += len(bodyBytes)
		if decodedLimit > 0 && decodedSize > decodedLimit {
			return nil, ErrTooLarge
		}
		if mediaType == "text/html" {
			htmlBody = string(bodyBytes)
			links = append(links, extractLinks(htmlBody)...)
		} else {
			textBody = string(bodyBytes)
			links = append(links, extractLinks(textBody)...)
		}
	}

	parsed.TextBody = textBody
	parsed.HTMLBody = htmlBody
	parsed.Attachments = attachments
	parsed.Links = dedupeLinks(links)
	parsed.DecodedSize = decodedSize

	if decodedLimit > 0 && decodedSize > decodedLimit {
		return nil, ErrTooLarge
	}

	return parsed, nil
}

func readPartBody(r io.Reader, enc string) ([]byte, error) {
	var reader io.Reader = r
	switch enc {
	case "base64":
		// MIME base64 may contain newlines; use decoder that ignores whitespace.
		// Wrap in a reader that strips whitespace? base64 decoder handles newlines if we use StdEncoding with strict?
		// Simpler: read all, strip whitespace, then decode.
		b, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		// Remove whitespace (CR, LF, space, tab)
		b = bytes.ReplaceAll(b, []byte("\r"), nil)
		b = bytes.ReplaceAll(b, []byte("\n"), nil)
		b = bytes.ReplaceAll(b, []byte(" "), nil)
		b = bytes.ReplaceAll(b, []byte("\t"), nil)
		if len(b) == 0 {
			return []byte{}, nil
		}
		decoded := make([]byte, base64.StdEncoding.DecodedLen(len(b)))
		n, err := base64.StdEncoding.Decode(decoded, b)
		if err != nil {
			// Try raw std encoding without padding
			decoded2 := make([]byte, base64.RawStdEncoding.DecodedLen(len(b)))
			n2, err2 := base64.RawStdEncoding.Decode(decoded2, b)
			if err2 != nil {
				return nil, ErrValidation
			}
			return decoded2[:n2], nil
		}
		return decoded[:n], nil
	case "quoted-printable":
		qpr := quotedprintable.NewReader(r)
		b, err := io.ReadAll(qpr)
		if err != nil {
			return nil, ErrValidation
		}
		return b, nil
	default:
		b, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		return b, nil
	}
}

func decodeWordSafe(s string) string {
	dec := new(mime.WordDecoder)
	decoded, err := dec.DecodeHeader(s)
	if err != nil {
		return s
	}
	return decoded
}

func extractAddrSpec(headerFrom string) (string, string) {
	if headerFrom == "" {
		return "", ""
	}
	// Use net/mail to parse address. It handles "Name <addr>" and "addr".
	addr, err := mail.ParseAddress(headerFrom)
	if err != nil {
		// Fallback: try to find email via regex
		re := regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
		m := re.FindString(headerFrom)
		if m == "" {
			return strings.ToLower(strings.TrimSpace(headerFrom)), ""
		}
		m = strings.ToLower(m)
		parts := strings.SplitN(m, "@", 2)
		if len(parts) == 2 {
			return m, strings.ToLower(parts[1])
		}
		return m, ""
	}
	// addr.Address is the addr-spec; lowercased for exact matching per spec.
	lower := strings.ToLower(strings.TrimSpace(addr.Address))
	parts := strings.SplitN(lower, "@", 2)
	domain := ""
	if len(parts) == 2 {
		domain = parts[1]
	}
	return lower, domain
}

var linkRe = regexp.MustCompile(`https?://[^\s"'<>\)]+`)

func extractLinks(text string) []string {
	matches := linkRe.FindAllString(text, -1)
	// Trim trailing punctuation that is not part of URL.
	for i, m := range matches {
		m = strings.TrimRight(m, ".,;!?)")
		// Remove trailing single quote or double quote leftovers
		m = strings.Trim(m, "'\"")
		matches[i] = m
	}
	return matches
}

func dedupeLinks(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	var out []string
	for _, l := range in {
		if l == "" {
			continue
		}
		if _, ok := seen[l]; !ok {
			seen[l] = struct{}{}
			out = append(out, l)
		}
	}
	return out
}

// envelope extraction helpers for tests: parse raw to get envelope from header if not supplied.
func parseEnvelopeFrom(raw []byte) string {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	// Prefer Return-Path or Envelope-From if present, else From header address.
	if rp := msg.Header.Get("Return-Path"); rp != "" {
		if a, err := mail.ParseAddress(rp); err == nil {
			return strings.ToLower(strings.TrimSpace(a.Address))
		}
	}
	from := msg.Header.Get("From")
	if a, err := mail.ParseAddress(from); err == nil {
		return strings.ToLower(strings.TrimSpace(a.Address))
	}
	return ""
}

// For strict size accounting, also expose decoded size calculation helper.
func decodedSizeOf(parsed *ParsedMessage) int {
	sz := len(parsed.TextBody) + len(parsed.HTMLBody)
	for _, att := range parsed.Attachments {
		sz += len(att.Data)
	}
	return sz
}
