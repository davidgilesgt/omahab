import { describe, it, expect } from "vitest";
import {
  buildCanonicalBytes,
  base64Encode,
  base64Decode,
  toHex,
  fromHex,
  isDecimalUintString,
} from "./canonical";

describe("buildCanonicalBytes", () => {
  it("is deterministic and version-prefixed", () => {
    const meta = {
      from: "alice@hey.com",
      to: "ai@example.com",
      timestamp: "1710000000",
      nonce: "abc123",
    };
    const raw = new TextEncoder().encode("From: alice@hey.com\r\n\r\nhello");
    const a = buildCanonicalBytes(meta, raw);
    const b = buildCanonicalBytes(meta, raw);
    expect(a).toEqual(b);
    const text = new TextDecoder().decode(a);
    expect(text.startsWith("v1\n1710000000\nabc123\nalice@hey.com\nai@example.com\n")).toBe(true);
    expect(text.endsWith("hello")).toBe(true);
    expect(text).toContain(`${raw.length}\n`);
  });

  it("covers all metadata and raw length exactly", () => {
    const meta = {
      from: "a@b.c",
      to: "ai@example.com",
      timestamp: "100",
      nonce: "n1",
    };
    const raw = new TextEncoder().encode("RAW_BYTES");
    const canonical = buildCanonicalBytes(meta, raw);
    const header = `v1\n${meta.timestamp}\n${meta.nonce}\n${meta.from}\n${meta.to}\n${raw.length}\n`;
    const expected = new TextEncoder().encode(header);
    const combined = new Uint8Array(expected.length + raw.length);
    combined.set(expected, 0);
    combined.set(raw, expected.length);
    expect(canonical).toEqual(combined);
  });

  it("raw bytes are appended verbatim without transform", () => {
    const meta = { from: "x@y.z", to: "ai@example.com", timestamp: "1", nonce: "n" };
    // Include bytes that would be ambiguous if header used naive concatenation
    const raw = new Uint8Array([0x00, 0xff, 0x0a, 0x0d, 0x00]);
    const canonical = buildCanonicalBytes(meta, raw);
    const header = `v1\n1\nn\nx@y.z\nai@example.com\n${raw.length}\n`;
    const headerBytes = new TextEncoder().encode(header);
    expect(canonical.slice(headerBytes.length)).toEqual(raw);
    expect(canonical.slice(0, headerBytes.length)).toEqual(headerBytes);
  });

  it("different nonce changes canonical", () => {
    const raw = new TextEncoder().encode("body");
    const a = buildCanonicalBytes({ from: "a@b.c", to: "ai@example.com", timestamp: "1", nonce: "aaa" }, raw);
    const b = buildCanonicalBytes({ from: "a@b.c", to: "ai@example.com", timestamp: "1", nonce: "bbb" }, raw);
    expect(a).not.toEqual(b);
  });

  it("different from/to changes canonical", () => {
    const raw = new TextEncoder().encode("body");
    const a = buildCanonicalBytes({ from: "alice@hey.com", to: "ai@example.com", timestamp: "1", nonce: "n" }, raw);
    const b = buildCanonicalBytes({ from: "bob@hey.com", to: "ai@example.com", timestamp: "1", nonce: "n" }, raw);
    expect(a).not.toEqual(b);
  });

  it("empty raw produces header only", () => {
    const meta = { from: "a@b.c", to: "ai@example.com", timestamp: "0", nonce: "n" };
    const raw = new Uint8Array(0);
    const canonical = buildCanonicalBytes(meta, raw);
    const header = `v1\n0\nn\na@b.c\nai@example.com\n0\n`;
    expect(new TextDecoder().decode(canonical)).toBe(header);
  });

  it("HMAC over canonical bytes covers raw exactly (integration check)", async () => {
    // Simulate HMAC to prove that tampering raw changes signature
    const meta = { from: "alice@hey.com", to: "ai@example.com", timestamp: "999", nonce: "deadbeef" };
    const raw = new TextEncoder().encode("original");
    const tampered = new TextEncoder().encode("tampered");
    const secret = "test-secret-with-sufficient-length";
    const keyData = new TextEncoder().encode(secret);
    const key = await crypto.subtle.importKey("raw", Uint8Array.from(keyData).buffer, { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
    const a = buildCanonicalBytes(meta, raw);
    const b = buildCanonicalBytes(meta, tampered);
    const sigA = new Uint8Array(await crypto.subtle.sign("HMAC", key, Uint8Array.from(a).buffer));
    const sigB = new Uint8Array(await crypto.subtle.sign("HMAC", key, Uint8Array.from(b).buffer));
    expect(sigA).not.toEqual(sigB);
  });
});

describe("base64 helpers", () => {
  it("round-trips arbitrary bytes", () => {
    const data = new Uint8Array([0, 1, 2, 255, 254, 13, 10, 0]);
    expect(base64Decode(base64Encode(data))).toEqual(data);
  });

  it("empty encodes to empty", () => {
    expect(base64Encode(new Uint8Array(0))).toBe("");
  });
});

describe("hex helpers", () => {
  it("round-trips", () => {
    const bytes = new Uint8Array([0, 15, 16, 255, 171]);
    expect(fromHex(toHex(bytes))).toEqual(bytes);
  });
});

describe("isDecimalUintString", () => {
  it("validates decimal strings", () => {
    expect(isDecimalUintString("0")).toBe(true);
    expect(isDecimalUintString("123")).toBe(true);
    expect(isDecimalUintString("")).toBe(false);
    expect(isDecimalUintString("-1")).toBe(false);
    expect(isDecimalUintString("12.3")).toBe(false);
    expect(isDecimalUintString("abc")).toBe(false);
  });
});
