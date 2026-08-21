/**
 * Unit-testable canonicalization for the Omahab Email Worker.
 *
 * HMAC-SHA256 is computed over the canonical byte sequence that
 * concatenates versioned metadata (timestamp, nonce, from, to, raw length)
 * and the raw RFC 5321/5322 message bytes verbatim. The canonical form
 * exactly covers every byte forwarded to the ingestion endpoint:
 *  - all forwarded metadata fields (from, to, timestamp, nonce)
 *  - the raw message body
 *
 * No MIME parsing, trust decisions, or routing logic live here.
 */

export interface CanonicalMetadata {
  /** Envelope MAIL FROM, as presented by Cloudflare (message.from). */
  from: string;
  /** Envelope RCPT TO, as presented by Cloudflare (message.to). */
  to: string;
  /** Unix seconds as decimal string, generated at the edge. */
  timestamp: string;
  /** Cryptographic nonce (hex or UUID), generated at the edge. */
  nonce: string;
}

const VERSION = "v1";

/**
 * Build the canonical byte sequence that is HMAC-SHA256 authenticated.
 *
 * Format (all metadata fields are UTF-8, LF-delimited, version-prefixed):
 *   v1\n
 *   <timestamp>\n
 *   <nonce>\n
 *   <from>\n
 *   <to>\n
 *   <raw.length as decimal>\n
 *   <raw bytes>
 *
 * - `raw.length` is the decimal byte-length of `raw`. Including it
 *   prevents ambiguity where metadata newline boundaries could be
 *   confused with raw content. The raw bytes are appended verbatim
 *   without transformation, so HMAC covers them exactly.
 * - No trimming or case-folding is applied here; callers normalize
 *   recipient/sender before calling if desired. Canonicalization
 *   is intentionally byte-exact to remain forge-resistant.
 * - Version prefix allows future evolution without silent downgrade.
 */
export function buildCanonicalBytes(
  metadata: CanonicalMetadata,
  raw: Uint8Array,
): Uint8Array {
  const encoder = new TextEncoder();
  const header = `${VERSION}\n${metadata.timestamp}\n${metadata.nonce}\n${metadata.from}\n${metadata.to}\n${raw.length}\n`;
  const headerBytes = encoder.encode(header);
  const out = new Uint8Array(headerBytes.length + raw.length);
  out.set(headerBytes, 0);
  out.set(raw, headerBytes.length);
  return out;
}

/**
 * Convenience helper that returns the canonical bytes as a UTF-8 string
 * for debug display when raw is known to be ASCII. For binary raw,
 * use buildCanonicalBytes directly. This is not used for HMAC.
 */
export function buildCanonicalString(
  metadata: CanonicalMetadata,
  rawText: string,
): string {
  const raw = new TextEncoder().encode(rawText);
  const canonical = buildCanonicalBytes(metadata, raw);
  // Header is ASCII; rawText assumed ASCII here.
  return new TextDecoder().decode(canonical);
}

/**
 * Constant-time helpers for validation/formatting.
 */

export function isDecimalUintString(value: string): boolean {
  return /^[0-9]+$/.test(value) && value.length > 0 && value.length <= 20;
}

export function toHex(bytes: Uint8Array): string {
  let hex = "";
  for (let i = 0; i < bytes.length; i++) {
    hex += bytes[i].toString(16).padStart(2, "0");
  }
  return hex;
}

export function fromHex(hex: string): Uint8Array {
  if (hex.length % 2 !== 0) throw new Error("invalid hex length");
  const out = new Uint8Array(hex.length / 2);
  for (let i = 0; i < out.length; i++) {
    const byte = parseInt(hex.slice(i * 2, i * 2 + 2), 16);
    if (Number.isNaN(byte)) throw new Error("invalid hex");
    out[i] = byte;
  }
  return out;
}

/** Encode bytes to base64 (standard, not url-safe), for JSON transport. */
export function base64Encode(bytes: Uint8Array): string {
  // Use chunked String.fromCharCode to avoid stack blowup on large inputs.
  let binary = "";
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    const slice = bytes.subarray(i, i + chunk);
    binary += String.fromCharCode(...slice);
  }
  return btoa(binary);
}

export function base64Decode(b64: string): Uint8Array {
  const binary = atob(b64);
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) out[i] = binary.charCodeAt(i);
  return out;
}
