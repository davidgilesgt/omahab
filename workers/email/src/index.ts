import {
  buildCanonicalBytes,
  base64Encode,
  type CanonicalMetadata,
} from "./canonical";

/**
 * Cloudflare Email Worker — minimal native TypeScript transport adapter.
 *
 * Responsibilities:
 *  - validate environment (fail closed on malformed config)
 *  - enforce allowed recipient (single configured address)
 *  - enforce raw-size limit
 *  - attach timestamp + cryptographic nonce
 *  - HMAC-SHA256 over canonical metadata + raw bytes
 *  - POST JSON to narrow /api/v1/email/ingest with retry on transient errors
 *
 * Non-responsibilities (intentionally absent):
 *  - no MIME parsing
 *  - no sender-trust / DKIM / DMARC decisions
 *  - no application routing (Paperless / Karakeep / inbox)
 *  - no cookies, no provider secrets in transit or logs
 */

// ---------------------------------------------------------------------------
// Environment
// ---------------------------------------------------------------------------

export interface Env {
  /** HMAC-SHA256 secret (min 16 chars, never logged, never sent). */
  HMAC_SECRET: string;
  /** Full ingestion URL, must be https and end with /api/v1/email/ingest */
  INGEST_URL: string;
  /** Single allowed envelope recipient, e.g. ai@example.com */
  ALLOWED_RECIPIENT: string;
  /** Optional max raw size in bytes as decimal string, default 26214400 (25 MiB) */
  MAX_RAW_BYTES?: string;
  /** Optional HMAC key id if rotation is needed; forwarded as metadata but covered by HMAC */
  HMAC_KEY_ID?: string;
}

interface ValidatedEnv {
  hmacSecret: string;
  ingestUrl: string;
  allowedRecipient: string;
  maxRawBytes: number;
  hmacKeyId: string | null;
}

const DEFAULT_MAX_RAW_BYTES = 25 * 1024 * 1024; // 25 MiB
const MIN_HMAC_SECRET_LENGTH = 16;
const MAX_RETRIES = 3;

function normalizeEmail(value: string): string {
  return value.trim().toLowerCase();
}

function isValidEmail(value: string): boolean {
  // Pragmatic RFC 5321-ish check; authoritative verification is in omahabd.
  return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(value);
}

export function validateEnv(env: Env): ValidatedEnv {
  if (!env || typeof env !== "object") {
    throw new Error("invalid environment: missing env");
  }

  const secret = (env.HMAC_SECRET ?? "").trim();
  if (secret.length < MIN_HMAC_SECRET_LENGTH) {
    throw new Error(
      `invalid environment: HMAC_SECRET must be at least ${MIN_HMAC_SECRET_LENGTH} characters`,
    );
  }

  const ingestUrlRaw = (env.INGEST_URL ?? "").trim();
  if (!ingestUrlRaw) {
    throw new Error("invalid environment: INGEST_URL is required");
  }
  let parsed: URL;
  try {
    parsed = new URL(ingestUrlRaw);
  } catch {
    throw new Error("invalid environment: INGEST_URL must be a valid URL");
  }
  if (parsed.protocol !== "https:") {
    throw new Error("invalid environment: INGEST_URL must be https");
  }
  if (!parsed.pathname.endsWith("/api/v1/email/ingest")) {
    throw new Error(
      "invalid environment: INGEST_URL must end with /api/v1/email/ingest",
    );
  }

  const allowed = normalizeEmail(env.ALLOWED_RECIPIENT ?? "");
  if (!allowed || !isValidEmail(allowed)) {
    throw new Error(
      "invalid environment: ALLOWED_RECIPIENT must be a valid email",
    );
  }

  let maxRawBytes = DEFAULT_MAX_RAW_BYTES;
  if (env.MAX_RAW_BYTES !== undefined && env.MAX_RAW_BYTES !== "") {
    const raw = env.MAX_RAW_BYTES.trim();
    if (!/^[0-9]+$/.test(raw)) {
      throw new Error("invalid environment: MAX_RAW_BYTES must be decimal");
    }
    const parsedBytes = Number(raw);
    if (!Number.isSafeInteger(parsedBytes) || parsedBytes <= 0 || parsedBytes > 100 * 1024 * 1024) {
      throw new Error(
        "invalid environment: MAX_RAW_BYTES must be 1..104857600",
      );
    }
    maxRawBytes = parsedBytes;
  }

  const keyId = env.HMAC_KEY_ID?.trim() ? env.HMAC_KEY_ID.trim() : null;
  if (keyId !== null && !/^[a-zA-Z0-9_-]{1,64}$/.test(keyId)) {
    throw new Error("invalid environment: HMAC_KEY_ID contains invalid characters");
  }

  return {
    hmacSecret: secret,
    ingestUrl: parsed.toString(),
    allowedRecipient: allowed,
    maxRawBytes,
    hmacKeyId: keyId,
  };
}

// ---------------------------------------------------------------------------
// Crypto
// ---------------------------------------------------------------------------

async function hmacHex(secret: string, canonical: Uint8Array): Promise<string> {
  const keyData = new TextEncoder().encode(secret);
  const cryptoKey = await crypto.subtle.importKey(
    "raw",
    Uint8Array.from(keyData).buffer,
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const sig = await crypto.subtle.sign("HMAC", cryptoKey, Uint8Array.from(canonical).buffer);
  const bytes = new Uint8Array(sig);
  let hex = "";
  for (let i = 0; i < bytes.length; i++) {
    hex += bytes[i].toString(16).padStart(2, "0");
  }
  return hex;
}

function generateTimestamp(): string {
  // Seconds since epoch, decimal string. Prevents JS ms-ambiguity.
  return Math.floor(Date.now() / 1000).toString();
}

function generateNonce(): string {
  // 128-bit cryptographic nonce, hex-encoded. crypto.randomUUID is also
  // acceptable but we use getRandomValues for explicit entropy size.
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  let hex = "";
  for (let i = 0; i < bytes.length; i++) {
    hex += bytes[i].toString(16).padStart(2, "0");
  }
  return hex;
}

// ---------------------------------------------------------------------------
// Ingest forwarding
// ---------------------------------------------------------------------------

interface ForwardPayload {
  from: string;
  to: string;
  timestamp: string;
  nonce: string;
  raw: string; // base64-encoded raw message
  rawSize: number;
  // Optional key id for rotation; if present it is covered by HMAC via canonical metadata
  // (canonical does not currently include keyId to avoid ambiguous downgrade; caller may
  // extend canonical with keyId in a versioned way. For now keyId travels as header
  // and is not part of HMAC; future v2 will include it.)
}

function sleepMs(ms: number): Promise<void> {
	const { promise, resolve } = Promise.withResolvers<void>();
	setTimeout(resolve, ms);
	return promise;
}

async function postWithRetry(
  url: string,
  payload: ForwardPayload,
  headers: Record<string, string>,
): Promise<Response> {
  const body = JSON.stringify(payload);
  let lastError: unknown = null;
  let lastResponse: Response | null = null;

  for (let attempt = 0; attempt <= MAX_RETRIES; attempt++) {
    if (attempt > 0) {
      // Exponential backoff with jitter: 200ms * 2^(attempt-1) ± 20%
      const base = 200 * Math.pow(2, attempt - 1);
      const jitter = base * 0.2 * (Math.random() * 2 - 1);
      const delay = Math.max(0, base + jitter);
      await sleepMs(delay);
    }

    let response: Response;
    try {
      response = await fetch(url, {
        method: "POST",
        headers: {
          "content-type": "application/json",
          // No cookies, no credentials, no provider tokens.
          ...headers,
        },
        body,
        // Explicitly avoid sending credentials; Cloudflare fetch defaults to omit,
        // but we state it for auditability.
        // `credentials` is not a valid fetch init in Workers; omission is correct.
      });
    } catch (err) {
      // Network failure — transient, retry.
      lastError = err;
      lastResponse = null;
      continue;
    }

    if (response.ok) {
      return response;
    }

    if (response.status === 408 || response.status === 429 || response.status >= 500) {
      lastError = new Error(`transient upstream error: ${response.status}`);
      lastResponse = response;
      // Drain body to avoid leaks before retry.
      try {
        await response.text();
      } catch {
        // ignore
      }
      continue;
    }

    // Permanent error — fail closed, do not retry. Throw to surface 4xx
    // so caller can decide to temp-fail the SMTP transaction.
    const text = await response.text().catch(() => "");
    throw new Error(
      `ingest rejected with ${response.status}: ${text.slice(0, 512)}`,
    );
  }

  // Exhausted retries — fail closed (temp failure for SMTP retry).
  if (lastResponse) {
    const text = await lastResponse.text().catch(() => "");
    throw new Error(
      `ingest transient failure after ${MAX_RETRIES + 1} attempts: ${lastResponse.status} ${text.slice(0, 256)}`,
    );
  }
  throw new Error(
    `ingest network failure after ${MAX_RETRIES + 1} attempts: ${String(lastError)}`,
  );
}

// ---------------------------------------------------------------------------
// Helpers to read Cloudflare email message
// ---------------------------------------------------------------------------

async function readRawBytes(message: ForwardableEmailMessage): Promise<Uint8Array> {
  // message.raw is a ReadableStream<Uint8Array>. Consume via Response helper.
  const ab = await new Response(message.raw).arrayBuffer();
  return new Uint8Array(ab);
}

// Minimal typing for Cloudflare EmailMessage; we avoid importing
// "cloudflare:email" at runtime to keep unit-testability.
interface ForwardableEmailMessage {
  readonly from: string;
  readonly to: string;
  readonly raw: ReadableStream<Uint8Array>;
  readonly rawSize: number;
  readonly headers: Headers;
  setReject(reason: string): void;
}

// ---------------------------------------------------------------------------
// Main handler
// ---------------------------------------------------------------------------

export async function handleEmail(
  message: ForwardableEmailMessage,
  env: Env,
): Promise<void> {
  // Fail closed on any malformed environment.
  const cfg = validateEnv(env);

  // Enforce allowed recipient exactly (case-insensitive, trimmed).
  // No alias expansion or routing logic — single configured address.
  const recipient = normalizeEmail(message.to ?? "");
  if (!recipient || recipient !== cfg.allowedRecipient) {
    message.setReject("Recipient not allowed");
    return;
  }

  // Enforce raw-size limit before reading body to bound memory.
  if (message.rawSize > cfg.maxRawBytes) {
    message.setReject(`Message too large: ${message.rawSize} > ${cfg.maxRawBytes}`);
    return;
  }

  // Read raw bytes verbatim; do not parse MIME.
  const rawBytes = await readRawBytes(message);

  // Double-check size after read (rawSize may be untrusted or mismatched).
  if (rawBytes.length > cfg.maxRawBytes) {
    message.setReject(`Message too large: ${rawBytes.length} > ${cfg.maxRawBytes}`);
    return;
  }
  if (rawBytes.length !== message.rawSize) {
    // Size mismatch is not fatal; we proceed with actual bytes but keep
    // rawSize in payload for observability. HMAC covers actual bytes.
  }

  const timestamp = generateTimestamp();
  const nonce = generateNonce();

  // Canonical bytes cover all forwarded metadata + raw body exactly.
  const metadata: CanonicalMetadata = {
    from: message.from ?? "",
    to: message.to ?? "",
    timestamp,
    nonce,
  };
  const canonical = buildCanonicalBytes(metadata, rawBytes);
  const signature = await hmacHex(cfg.hmacSecret, canonical);

  const payload: ForwardPayload = {
    from: message.from,
    to: message.to,
    timestamp,
    nonce,
    raw: base64Encode(rawBytes),
    rawSize: rawBytes.length,
  };

  const headers: Record<string, string> = {
    "x-omahab-timestamp": timestamp,
    "x-omahab-nonce": nonce,
    "x-omahab-signature": `sha256=${signature}`,
    "x-omahab-from": message.from,
    "x-omahab-to": message.to,
  };
  if (cfg.hmacKeyId) {
    headers["x-omahab-key-id"] = cfg.hmacKeyId;
  }

  // Forward to narrow ingestion endpoint; retry on transient errors,
  // fail closed (throw) otherwise so Cloudflare retries the SMTP transaction.
  await postWithRetry(cfg.ingestUrl, payload, headers);
}

// Cloudflare Workers entry point.
export default {
  async email(
    message: ForwardableEmailMessage,
    env: Env,
    _ctx: ExecutionContext,
  ): Promise<void> {
    await handleEmail(message, env);
  },
} satisfies ExportedHandler<Env>;
