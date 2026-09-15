import { describe, it, expect, vi, afterEach } from "vitest";
import { createHmac } from "node:crypto";
import { handleEmail } from "./index";
import { buildCanonicalBytes } from "./canonical";

const ENV = {
  HMAC_SECRET: "test-key-0123456789abcdef",
  INGEST_URL: "https://api.example.com/api/v1/email/ingest",
  ALLOWED_RECIPIENT: "ai@example.com",
};

function message(to: string, rawText: string, rawSize?: number) {
  const raw = new TextEncoder().encode(rawText);
  const rejects: string[] = [];
  return {
    msg: {
      from: "sender@example.org",
      to,
      raw: new ReadableStream<Uint8Array>({
        start(c) {
          c.enqueue(raw);
          c.close();
        },
      }),
      rawSize: rawSize ?? raw.length,
      headers: new Headers(),
      setReject(reason: string) {
        rejects.push(reason);
      },
    },
    rejects,
    raw,
  };
}

function stubFetch(impl: (url: string, init: RequestInit) => Response | Promise<Response>) {
  const calls: { url: string; init: RequestInit }[] = [];
  vi.stubGlobal(
    "fetch",
    async (url: string, init: RequestInit) => {
      calls.push({ url, init });
      return impl(url, init);
    },
  );
  return calls;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("handleEmail", () => {
  it("posts once with an HMAC v1 signature over the exact canonical bytes", async () => {
    const { msg, raw } = message("ai@example.com", "hello");
    const calls = stubFetch(() => new Response("ok", { status: 200 }));

    await handleEmail(msg, ENV);

    expect(calls).toHaveLength(1);
    expect(calls[0].url).toBe(ENV.INGEST_URL);
    const headers = calls[0].init.headers as Record<string, string>;
    const body = JSON.parse(calls[0].init.body as string);
    expect(body.rawSize).toBe(5);
    expect(headers["x-omahab-signature"]).toMatch(/^sha256=[0-9a-f]{64}$/);
    // Recompute the signature independently over the canonical bytes.
    const canonical = buildCanonicalBytes(
      { from: "sender@example.org", to: "ai@example.com", timestamp: body.timestamp, nonce: body.nonce },
      raw,
    );
    const expected =
      "sha256=" + createHmac("sha256", ENV.HMAC_SECRET).update(canonical).digest("hex");
    expect(headers["x-omahab-signature"]).toBe(expected);
  });

  it("rejects a disallowed recipient without posting", async () => {
    const { msg, rejects } = message("intruder@evil.com", "hello");
    const calls = stubFetch(() => new Response("ok", { status: 200 }));

    await handleEmail(msg, ENV);

    expect(rejects).toEqual(["Recipient not allowed"]);
    expect(calls).toHaveLength(0);
  });

  it("rejects oversize input without posting", async () => {
    const { msg, rejects } = message("ai@example.com", "hello", 26_214_401);
    const calls = stubFetch(() => new Response("ok", { status: 200 }));

    await handleEmail(msg, ENV);

    expect(rejects).toHaveLength(1);
    expect(rejects[0]).toMatch(/too large/);
    expect(calls).toHaveLength(0);
  });

  it("makes exactly one attempt on HTTP 400", async () => {
    const { msg } = message("ai@example.com", "hello");
    const calls = stubFetch(() => new Response("bad", { status: 400 }));

    await expect(handleEmail(msg, ENV)).rejects.toThrow(/ingest rejected with 400/);
    expect(calls).toHaveLength(1);
  });

  it("retries transient failures and succeeds on the third attempt", async () => {
    const { msg } = message("ai@example.com", "hello");
    let n = 0;
    const calls = stubFetch(() => {
      n += 1;
      return new Response(n < 3 ? "busy" : "ok", { status: n < 3 ? 500 : 200 });
    });

    await handleEmail(msg, ENV);

    expect(calls).toHaveLength(3);
  });

  it("surfaces the last status after four transient failures", async () => {
    const { msg } = message("ai@example.com", "hello");
    const calls = stubFetch(() => new Response("down", { status: 503 }));

    await expect(handleEmail(msg, ENV)).rejects.toThrow(
      /ingest transient failure after 4 attempts: 503 down/,
    );
    expect(calls).toHaveLength(4);
  });
});
