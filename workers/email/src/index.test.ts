import { describe, it, expect } from "vitest";
import { validateEnv } from "./index";
describe("validateEnv", () => {
  const base = {
    HMAC_SECRET: "a-very-long-secret-1234567890",
    INGEST_URL: "https://api.example.com/api/v1/email/ingest",
    ALLOWED_RECIPIENT: "ai@example.com",
  };

  it("accepts valid env", () => {
    expect(validateEnv(base).allowedRecipient).toBe("ai@example.com");
  });

  it("fails closed on missing HMAC_SECRET", () => {
    expect(() => validateEnv({ ...base, HMAC_SECRET: "" })).toThrow(/HMAC_SECRET/);
    expect(() => validateEnv({ ...base, HMAC_SECRET: "short" })).toThrow(/HMAC_SECRET/);
  });

  it("fails closed on missing INGEST_URL", () => {
    expect(() => validateEnv({ ...base, INGEST_URL: "" })).toThrow(/INGEST_URL/);
  });

  it("fails closed on http ingest url", () => {
    expect(() => validateEnv({ ...base, INGEST_URL: "http://api.example.com/api/v1/email/ingest" })).toThrow(/https/);
  });

  it("fails closed when INGEST_URL does not end with narrow endpoint", () => {
    expect(() => validateEnv({ ...base, INGEST_URL: "https://api.example.com/api/v1/other" })).toThrow(/\/api\/v1\/email\/ingest/);
    expect(() => validateEnv({ ...base, INGEST_URL: "https://api.example.com/" })).toThrow();
  });

  it("fails closed on invalid allowed recipient", () => {
    expect(() => validateEnv({ ...base, ALLOWED_RECIPIENT: "" })).toThrow();
    expect(() => validateEnv({ ...base, ALLOWED_RECIPIENT: "not-an-email" })).toThrow();
  });

  it("normalizes allowed recipient case", () => {
    const cfg = validateEnv({ ...base, ALLOWED_RECIPIENT: "AI@EXAMPLE.COM" });
    expect(cfg.allowedRecipient).toBe("ai@example.com");
  });

  it("validates MAX_RAW_BYTES", () => {
    expect(validateEnv({ ...base, MAX_RAW_BYTES: "1024" }).maxRawBytes).toBe(1024);
    expect(() => validateEnv({ ...base, MAX_RAW_BYTES: "abc" })).toThrow();
    expect(() => validateEnv({ ...base, MAX_RAW_BYTES: "0" })).toThrow();
  });

  it("validates HMAC_KEY_ID chars", () => {
    expect(validateEnv({ ...base, HMAC_KEY_ID: "key-1" }).hmacKeyId).toBe("key-1");
    expect(() => validateEnv({ ...base, HMAC_KEY_ID: "bad id!" })).toThrow();
  });

  it("accepts optional RECIPIENT_ALIAS sharing domain", () => {
    const cfg = validateEnv({ ...base, RECIPIENT_ALIAS: "alias@example.com" });
    expect(cfg.recipientAlias).toBe("alias@example.com");
    expect(cfg.allowedRecipients.has("alias@example.com")).toBe(true);
    expect(cfg.allowedRecipients.has("ai@example.com")).toBe(true);
  });

  it("fails closed on invalid alias", () => {
    expect(() => validateEnv({ ...base, RECIPIENT_ALIAS: "not-an-email" })).toThrow();
    expect(() => validateEnv({ ...base, RECIPIENT_ALIAS: "ai@example.com" })).toThrow(/differ/);
  });

  it("fails closed when alias domain mismatches", () => {
    expect(() => validateEnv({ ...base, RECIPIENT_ALIAS: "alias@other.com" })).toThrow(/domain/);
  });
});
