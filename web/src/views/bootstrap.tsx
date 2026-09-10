import { useEffect, useRef, useState, type FormEvent } from "react";
import { CheckCircle2 } from "lucide-react";
import { PageHeader, Section } from "../components/ui";
import { Stepper, type StepDef } from "../components/stepper";
import { CopyButton } from "../components/copyButton";
import { useToast } from "../components/toast";
import { useAuth } from "../auth";
import { ApiClient } from "../api/client";
import { useNavigate } from "react-router-dom";

const STEPS: StepDef[] = [
  { id: "claim", label: "Claim" },
  { id: "access", label: "Access" },
  { id: "ready", label: "Ready" },
];

const BOOTSTRAP_POLL_MS = 3000;

type Step = "claim" | "access" | "ready";

interface BootstrapResponse {
  ok: boolean;
  error?: string;
}

async function bootstrapFetch(path: string, token: string | null, body?: unknown, signal?: AbortSignal): Promise<BootstrapResponse & Record<string, unknown>> {
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (token) headers.Authorization = `Bearer ${token}`;
  const resp = await fetch(`/api/bootstrap/${path}`, {
    method: body !== undefined ? "POST" : "GET",
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
    signal,
  });
  const data = (await resp.json().catch(() => ({}))) as Record<string, unknown>;
  if (!resp.ok) {
    const nested = (data as unknown as { error?: { message?: string } })?.error?.message;
    const msg =
      typeof nested === "string" && nested
        ? nested
        : typeof (data as unknown as { message?: string })?.message === "string"
          ? (data as unknown as { message?: string })?.message
          : `HTTP ${resp.status}`;
    const err = new Error(msg) as Error & { status?: number };
    (err as unknown as { status: number }).status = resp.status;
    throw err;
  }
  return data as BootstrapResponse & Record<string, unknown>;
}

export function BootstrapPage() {
  const toast = useToast();
  const auth = useAuth();
  const navigate = useNavigate();
  const [step, setStep] = useState<Step>("claim");
  const [code, setCode] = useState("");
  const [codeError, setCodeError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [validating, setValidating] = useState(false);
  const pollRef = useRef<number | null>(null);

  const [installedKeys, setInstalledKeys] = useState<Array<{ fingerprint: string; type: string; comment: string }>>([]);
  const [installedUsername, setInstalledUsername] = useState<string>("");
  const [sshListLoading, setSshListLoading] = useState(false);
  const [sshListError, setSshListError] = useState<string | null>(null);
  const [showAddForm, setShowAddForm] = useState(false);
  const [githubUser, setGithubUser] = useState("");
  const [pastedKeys, setPastedKeys] = useState("");
  const [finishError, setFinishError] = useState<string | null>(null);

  const [tsAuthUrl, setTsAuthUrl] = useState<string | null>(null);
  const [tsRunning, setTsRunning] = useState(false);
  const [tsIp, setTsIp] = useState("");
  const [tsState, setTsState] = useState<string>("");
  const [tsError, setTsError] = useState<string | null>(null);
  const [tsOptIn, setTsOptIn] = useState(false);
  const [tsStarting, setTsStarting] = useState(false);
  const tsAbortRef = useRef<AbortController | null>(null);
  const tsReqRef = useRef(0);

  const token = auth.token;

  useEffect(() => () => {
    if (pollRef.current) window.clearInterval(pollRef.current);
    tsAbortRef.current?.abort();
  }, []);

  // Prefill from URL fragment only, strip fragment, require Claim click (no auto-claim)
  useEffect(() => {
    let codeFromHash = "";
    try {
      const hash = window.location.hash || "";
      if (hash) {
        const raw = hash.startsWith("#") ? hash.slice(1) : hash;
        const hp = new URLSearchParams(raw);
        codeFromHash = hp.get("code") || "";
        if (!codeFromHash && raw.startsWith("code=")) {
          codeFromHash = raw.slice(5).split("&")[0] || "";
        }
        if (!codeFromHash && raw.includes("code=")) {
          const m = raw.match(/code=([^&]+)/);
          if (m && m[1]) codeFromHash = decodeURIComponent(m[1]);
        }
      }
      // ignore query param intentionally - only fragment
      if (codeFromHash) {
        setCode(codeFromHash.trim());
      }
      // strip fragment regardless if it contained code
      if (hash.includes("code=")) {
        try {
          history.replaceState(null, "", window.location.pathname + window.location.search);
        } catch {}
      }
    } catch {}
  }, []);

  // Refresh-resumable: if we have a stored token, validate via authenticated SSH-key read
  useEffect(() => {
    if (!token) return;
    // only validate if we're still at claim step; if we already have token we should go to access
    let cancelled = false;
    setValidating(true);
    (async () => {
      try {
        const resp = await fetch("/api/bootstrap/ssh-keys", {
          headers: { Authorization: `Bearer ${token}` },
        });
        if (cancelled) return;
        if (!resp.ok) {
          let detail: unknown;
          try {
            detail = await resp.json();
          } catch {}
          const msg = (detail as { error?: { message?: string } })?.error?.message ?? `HTTP ${resp.status}`;
          if (resp.status === 401) {
            // rejected token clears session -> claim
            auth.signOut();
            setStep("claim");
          } else {
            // other errors: stay at claim but keep token? For safety, stay at claim
            // Could also be bootstrap already complete -> App would have redirected, but handle
            if (resp.status === 404) {
              // bootstrap complete, App should redirect, but if we're still here treat as ready?
            }
            setStep("claim");
          }
          throw new Error(msg);
        }
        setStep("access");
      } catch {
        // keep at claim if validation failed
      } finally {
        if (!cancelled) setValidating(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [token]);

  // Fetch installed SSH keys when entering access
  useEffect(() => {
    if (step !== "access" || !token) return;
    let cancelled = false;
    setSshListLoading(true);
    setSshListError(null);
    (async () => {
      try {
        const data = await bootstrapFetch("ssh-keys", token);
        if (cancelled) return;
        const itemsRaw = (data as unknown as { items?: unknown }).items;
        const items = Array.isArray(itemsRaw) ? itemsRaw : [];
        const usernameRaw = (data as unknown as { username?: unknown }).username;
        const username = typeof usernameRaw === "string" ? usernameRaw : "";
        const mapped = items
          .filter((it): it is Record<string, unknown> => typeof it === "object" && it !== null)
          .map((it) => {
            const fp = typeof (it as Record<string, unknown>).fingerprint === "string" ? String((it as Record<string, unknown>).fingerprint) : "";
            const tp = typeof (it as Record<string, unknown>).type === "string" ? String((it as Record<string, unknown>).type) : "";
            const cm = typeof (it as Record<string, unknown>).comment === "string" ? String((it as Record<string, unknown>).comment) : "";
            return { fingerprint: fp, type: tp, comment: cm };
          })
          .filter((k) => k.fingerprint);
        setInstalledKeys(mapped);
        setInstalledUsername(username);
        setShowAddForm(mapped.length === 0);
      } catch (err) {
        if (cancelled) return;
        setSshListError(err instanceof Error ? err.message : "failed to load keys");
      } finally {
        if (!cancelled) setSshListLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [step, token]);

  async function submitCode(e: FormEvent) {
    e.preventDefault();
    setCodeError(null);
    const trimmed = code.trim();
    if (!trimmed) {
      setCodeError("Code is required");
      return;
    }
    setBusy(true);
    try {
      const data = await bootstrapFetch("claim", null, { code: trimmed });
      const tokenVal = (data as unknown as { token?: unknown }).token;
      const t = typeof tokenVal === "string" ? tokenVal : null;
      if (!t) throw new Error("no token in response");
      auth.signIn(t);
      setStep("access");
      toast.success("Claimed");
    } catch (err) {
      const msg = err instanceof Error ? err.message : "claim failed";
      setCodeError(msg);
    } finally {
      setBusy(false);
    }
  }

  async function submitKeys(e: FormEvent) {
    e.preventDefault();
    if (!token) return;
    setBusy(true);
    try {
      await bootstrapFetch("ssh-keys", token, {
        github_user: githubUser.trim() || undefined,
        keys: pastedKeys.trim() ? pastedKeys.trim().split("\n").map((k) => k.trim()).filter(Boolean) : [],
      });
      // Refresh list
      try {
        const data = await bootstrapFetch("ssh-keys", token);
        const itemsRaw = (data as unknown as { items?: unknown }).items;
        const items = Array.isArray(itemsRaw) ? itemsRaw : [];
        const mapped = items
          .filter((it): it is Record<string, unknown> => typeof it === "object" && it !== null)
          .map((it) => {
            const fp = typeof (it as Record<string, unknown>).fingerprint === "string" ? String((it as Record<string, unknown>).fingerprint) : "";
            const tp = typeof (it as Record<string, unknown>).type === "string" ? String((it as Record<string, unknown>).type) : "";
            const cm = typeof (it as Record<string, unknown>).comment === "string" ? String((it as Record<string, unknown>).comment) : "";
            return { fingerprint: fp, type: tp, comment: cm };
          })
          .filter((k) => k.fingerprint);
        setInstalledKeys(mapped);
      } catch {}
      setGithubUser("");
      setPastedKeys("");
      toast.success("Keys installed");
      setShowAddForm(false);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "failed to install keys");
    } finally {
      setBusy(false);
    }
  }

  async function startTailscale() {
    if (!token || tsStarting) return;
    const req = ++tsReqRef.current;
    const ctrl = new AbortController();
    tsAbortRef.current?.abort();
    tsAbortRef.current = ctrl;
    const timer = window.setTimeout(() => ctrl.abort(), 35000);
    setTsOptIn(true);
    setTsStarting(true);
    setTsError(null);
    try {
      const data = await bootstrapFetch("tailscale/up", token, undefined, ctrl.signal);
      if (tsReqRef.current !== req) return; // superseded by "Set up later"
      const url = typeof data.auth_url === "string" ? data.auth_url : "";
      setTsAuthUrl(url || null);
      if (!url) {
        // Already enrolled (or nothing to do): confirm via one status poll.
        try {
          const st = await bootstrapFetch("tailscale/status", token);
          if (tsReqRef.current !== req) return;
          setTsRunning(st.running === true);
          if (typeof st.ip === "string") setTsIp(st.ip);
          if (typeof st.state === "string") setTsState(st.state);
        } catch {}
      }
      if (pollRef.current) window.clearInterval(pollRef.current);
      pollRef.current = window.setInterval(async () => {
        try {
          const st = await bootstrapFetch("tailscale/status", token);
          if (tsReqRef.current !== req) return;
          const running = st.running === true;
          const ip = typeof st.ip === "string" ? st.ip : "";
          const state = typeof st.state === "string" ? st.state : "";
          setTsRunning(running);
          setTsIp(ip);
          setTsState(state);
          if (running && ip && pollRef.current) {
            window.clearInterval(pollRef.current);
            pollRef.current = null;
          }
        } catch (err) {
          if (tsReqRef.current !== req) return;
          setTsError(err instanceof Error ? err.message : "status failed");
        }
      }, BOOTSTRAP_POLL_MS);
    } catch (err) {
      if (tsReqRef.current !== req) return;
      if (err instanceof DOMException && err.name === "AbortError") {
        setTsError("Timed out waiting for Tailscale — check the server can reach login.tailscale.com, then retry.");
      } else {
        setTsError(err instanceof Error ? err.message : "tailscale up failed");
      }
    } finally {
      window.clearTimeout(timer);
      if (tsReqRef.current === req) setTsStarting(false);
    }
  }

  function stopTailscalePoll() {
    tsReqRef.current++;
    tsAbortRef.current?.abort();
    tsAbortRef.current = null;
    if (pollRef.current) {
      window.clearInterval(pollRef.current);
      pollRef.current = null;
    }
    setTsStarting(false);
    setTsOptIn(false);
  }

  async function handleFinish() {
    if (!token) return;
    setBusy(true);
    setFinishError(null);
    try {
      await bootstrapFetch("complete", token);
      setStep("ready");
      toast.success("Setup complete");
    } catch (err) {
      // Dropped completion: check status to see if bootstrap is now complete
      try {
        const client = new ApiClient(() => null);
        const st = await client.bootstrapStatus();
        if (!st.active) {
          setStep("ready");
          toast.success("Setup complete");
          return;
        }
      } catch {}
      const msg = err instanceof Error ? err.message : "Failed to complete";
      setFinishError(msg);
      toast.error(msg);
    } finally {
      setBusy(false);
    }
  }

  const origin = typeof window !== "undefined" ? window.location.origin : "";

  return (
    <div className="page bootstrap-onboarding">
      <style>{`
        .bootstrap-onboarding {
          --paper: #181a1b;
          --surface: #222526;
          --ink: #eef0ed;
          --ink-muted: #a7aeaa;
          --line: #3a403d;
          --accent: #7fa38e;
          background: var(--paper);
          color: var(--ink);
          min-height: 100vh;
        }
        .bootstrap-onboarding .page-header h1 { font-size: 1.75rem; color: var(--ink); }
        .bootstrap-onboarding .page-header p { color: var(--ink-muted); }
        .bootstrap-onboarding .section { background: var(--surface); border: 1px solid var(--line); border-radius: 10px; padding: 20px; max-width: 100%; }
        .bootstrap-onboarding .field input, .bootstrap-onboarding .field textarea { background: var(--paper); color: var(--ink); border-color: var(--line); font-family: "IBM Plex Mono", monospace; }
        .bootstrap-onboarding .button.primary { background: var(--accent); color: #181a1b; border-color: var(--accent); }
        .bootstrap-onboarding .inline-error { color: #e8a0a0; display:flex; gap:6px; align-items:center; font-size:0.9em; }
        .bootstrap-onboarding .onboarding-grid-bg {
          background-image: linear-gradient(var(--line) 1px, transparent 1px), linear-gradient(90deg, var(--line) 1px, transparent 1px);
          background-size: 24px 24px;
          opacity: 0.06;
          position: fixed; inset:0; pointer-events:none;
        }
        .bootstrap-onboarding .reveal { animation: onboarding-reveal 0.4s ease-out; }
        @keyframes onboarding-reveal { from { opacity:0; transform: translateY(6px);} to { opacity:1; transform: translateY(0);} }
        @media (prefers-reduced-motion: reduce) { .bootstrap-onboarding .reveal { animation: none; } }
        @media (max-width: 360px) {
          .bootstrap-onboarding .form-actions { flex-direction: column; }
          .bootstrap-onboarding .form-actions .button { width: 100%; }
          .bootstrap-onboarding .field { width: 100%; }
        }
        .bootstrap-onboarding .mono { font-family: "IBM Plex Mono", monospace; }
        .bootstrap-onboarding a.mono { overflow-wrap: anywhere; word-break: break-all; }
        .bootstrap-onboarding .bootstrap-wrap { width: min(100%, 48rem); margin-inline: auto; padding: 24px 20px 48px; display: grid; gap: 20px; }
        .bootstrap-onboarding .page-header { flex-wrap: wrap; }
        .bootstrap-onboarding .form-grid { display: grid; gap: 16px; }
        .bootstrap-onboarding .field { display: grid; gap: 6px; }
        .bootstrap-onboarding .field > span { color: var(--ink-muted); font-size: 0.875rem; }
        .bootstrap-onboarding .form-actions { display: flex; gap: 12px; flex-wrap: wrap; align-items: center; }
        .bootstrap-onboarding .callout { border: 1px solid var(--line); border-radius: 6px; padding: 12px; background: var(--paper); display: grid; gap: 8px; }
        .bootstrap-onboarding .token-box { display: grid; gap: 8px; padding: 12px; border: 1px dashed var(--line); border-radius: 6px; background: var(--paper); }
      `}</style>
      <div className="onboarding-grid-bg" aria-hidden />
      <div className="bootstrap-wrap">
      <PageHeader
        eyebrow="Omahab"
        title={step === "claim" ? "Claim your server" : step === "access" ? "Secure access" : "Ready"}
        description={
          step === "claim"
            ? "Enter the one-time code shown on the server console."
            : step === "access"
              ? "Add SSH keys and optionally connect to your tailnet. You can finish without keys — password login remains over local console/SSH."
              : "Your control panel is ready at this address."
        }
      />
      <div className="reveal">
        <Stepper steps={STEPS} current={step} />
      </div>

      {step === "claim" && (
        <Section title="Enter the one-time code" description="The code is in the URL fragment and never sent as a query parameter. It is prefilled if you opened the QR link.">
          <form className="form-grid reveal" onSubmit={submitCode} noValidate>
            <label className="field">
              <span>One-time code</span>
              <input
                value={code}
                onChange={(e) => {
                  setCode(e.target.value);
                  if (codeError) setCodeError(null);
                }}
                placeholder="e.g. 7gc3x9k2mq"
                autoFocus
                className="mono"
                autoComplete="off"
                aria-invalid={codeError ? "true" : "false"}
                aria-describedby={codeError ? "code-error" : undefined}
              />
            </label>
            {codeError && (
              <p id="code-error" className="inline-error" role="alert" aria-live="polite">
                <span aria-hidden>⚠</span> {codeError}
              </p>
            )}
            <div className="form-actions">
              <button className="button primary" type="submit" disabled={busy || validating}>
                {busy ? "Claiming…" : validating ? "Checking…" : "Claim"}
              </button>
            </div>
            <p className="muted" style={{ fontSize: "0.85em", overflowWrap: "anywhere" }}>
              If you lost the tab, your browser session will resume while setup is incomplete. If you need a new code, run <code className="mono">sudo systemctl restart omahabd</code> on the host — this issues a new code only while setup is incomplete. Never delete <code className="mono">control.db</code>, <code className="mono">api.token</code> or <code className="mono">bootstrap-done</code> to fix access; after completion, sign in with the provisioned owner token.
            </p>
          </form>
        </Section>
      )}

      {step === "access" && (
        <Section
          title="Administrator SSH keys"
          description={`Keys for ${installedUsername || "administrator"} — console access remains the recovery path.`}
        >
          <div className="reveal" style={{ display: "grid", gap: 16 }}>
            {sshListLoading ? (
              <p className="muted">Loading installed keys…</p>
            ) : sshListError ? (
              <div>
                <p className="inline-error" role="alert">
                  <span aria-hidden>⚠</span> {sshListError}
                </p>
                <button
                  className="button ghost"
                  type="button"
                  onClick={() => {
                    setSshListLoading(true);
                    setSshListError(null);
                    void bootstrapFetch("ssh-keys", token)
                      .then((data) => {
                        const itemsRaw = (data as unknown as { items?: unknown }).items;
                        const items = Array.isArray(itemsRaw) ? itemsRaw : [];
                        const usernameRaw = (data as unknown as { username?: unknown }).username;
                        const username = typeof usernameRaw === "string" ? usernameRaw : "";
                        const mapped = items
                          .filter((it): it is Record<string, unknown> => typeof it === "object" && it !== null)
                          .map((it) => {
                            const fp = typeof (it as Record<string, unknown>).fingerprint === "string" ? String((it as Record<string, unknown>).fingerprint) : "";
                            const tp = typeof (it as Record<string, unknown>).type === "string" ? String((it as Record<string, unknown>).type) : "";
                            const cm = typeof (it as Record<string, unknown>).comment === "string" ? String((it as Record<string, unknown>).comment) : "";
                            return { fingerprint: fp, type: tp, comment: cm };
                          })
                          .filter((k) => k.fingerprint);
                        setInstalledKeys(mapped);
                        setInstalledUsername(username);
                        setShowAddForm(mapped.length === 0);
                      })
                      .catch((err) => setSshListError(err instanceof Error ? err.message : "failed to load keys"))
                      .finally(() => setSshListLoading(false));
                  }}
                >
                  Retry
                </button>
              </div>
            ) : installedKeys.length > 0 ? (
              <>
                <p className="muted">Already installed for <strong className="mono">{installedUsername || "administrator"}</strong>:</p>
                <ul style={{ listStyle: "none", padding: 0, display: "grid", gap: 6, marginTop: 8 }}>
                  {installedKeys.map((k) => (
                    <li
                      key={k.fingerprint}
                      style={{ display: "flex", gap: 8, alignItems: "center", fontSize: "0.9em", wordBreak: "break-all", overflowWrap: "anywhere" }}
                    >
                      <span className="mono">{k.type}</span>
                      <span className="mono">{k.fingerprint}</span>
                      <span style={{ opacity: 0.7 }}>{k.comment || "no comment"}</span>
                      <CopyButton text={k.fingerprint} label="Copy fingerprint" />
                    </li>
                  ))}
                </ul>
              </>
            ) : (
              <p className="muted">No SSH keys installed yet. You can finish without keys and add them later; password login works over local console and same-network SSH. Remember to replace any shared installation password with <code className="mono">passwd</code> over SSH or local login.</p>
            )}

            {!showAddForm ? (
              <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
                <button className="button secondary" type="button" onClick={() => setShowAddForm(true)} disabled={busy}>
                  Add a key
                </button>
              </div>
            ) : (
              <form className="form-grid" onSubmit={submitKeys} style={{ border: "1px solid var(--line)", borderRadius: 6, padding: 12 }}>
                <label className="field">
                  <span>GitHub username</span>
                  <input value={githubUser} onChange={(e) => setGithubUser(e.target.value)} placeholder="your-github-username" autoComplete="off" />
                </label>
                <label className="field">
                  <span>…or paste public keys (one per line)</span>
                  <textarea
                    value={pastedKeys}
                    onChange={(e) => setPastedKeys(e.target.value)}
                    rows={4}
                    placeholder="ssh-ed25519 AAAA…"
                    className="mono"
                  />
                </label>
                <div className="form-actions">
                  <button className="button ghost" type="button" onClick={() => setShowAddForm(installedKeys.length === 0 ? false : false)} disabled={busy}>
                    Cancel
                  </button>
                  <button className="button primary" type="submit" disabled={busy || (!githubUser.trim() && !pastedKeys.trim())}>
                    {busy ? "Adding…" : "Add keys"}
                  </button>
                </div>
              </form>
            )}

            <div style={{ borderTop: "1px solid var(--line)", paddingTop: 16, display: "grid", gap: 8 }}>
              <h3 style={{ margin: 0, fontSize: "1rem" }}>Tailscale (optional)</h3>
              {!tsOptIn ? (
                <div style={{ display: "flex", gap: 8, flexWrap: "wrap", alignItems: "center" }}>
                  <button className="button secondary" type="button" onClick={() => void startTailscale()} disabled={busy || tsStarting}>
                    {tsStarting ? "Starting…" : "Connect Tailscale"}
                  </button>
                  <span className="muted" style={{ fontSize: "0.85em" }}>Optional — you can connect your tailnet now or later from the dashboard.</span>
                </div>
              ) : (
                <div style={{ display: "grid", gap: 8, padding: 12, border: "1px solid var(--line)", borderRadius: 6 }}>
                  {tsAuthUrl ? (
                    <div className="callout">
                      <p>Open this URL to authorize the server:</p>
                      <p>
                        <a href={tsAuthUrl} target="_blank" rel="noreferrer" className="mono" style={{ overflowWrap: "anywhere" }}>
                          {tsAuthUrl}
                        </a>
                      </p>
                    </div>
                  ) : tsStarting ? (
                    <p className="muted">Contacting the server…</p>
                  ) : (
                    <p className="muted">No login URL was needed — checking status…</p>
                  )}
                  <p className="muted">
                    Status: {tsRunning ? "running" : "waiting for approval"}
                    {tsIp ? ` · ${tsIp}` : ""}
                    {tsState ? ` · ${tsState}` : ""}
                  </p>
                  {tsError && (
                    <p className="inline-error" role="alert">
                      <span aria-hidden>⚠</span> {tsError}
                    </p>
                  )}
                  <div style={{ display: "flex", gap: 8 }}>
                    <button className="button ghost" type="button" onClick={stopTailscalePoll}>
                      Set up later
                    </button>
                  </div>
                </div>
              )}
            </div>

            <p className="muted" style={{ fontSize: "0.85em" }}>
              If you used a shared installation password, replace it now: run <code className="mono">passwd</code> over SSH or local console login to set a strong unique password. SSH password authentication remains disabled; you will use keys or console for future access.
            </p>

            {finishError && (
              <p className="inline-error" role="alert">
                <span aria-hidden>⚠</span> {finishError}
              </p>
            )}
            <div className="form-actions">
              <button className="button primary" type="button" onClick={() => void handleFinish()} disabled={busy}>
                {busy ? "Finishing…" : "Finish setup"}
              </button>
            </div>
          </div>
        </Section>
      )}

      {step === "ready" && (
        <Section title="Your control panel is ready" description="Setup is complete at this origin. Bundled apps may still need domain/HTTPS configuration and backups are not configured yet.">
          <div className="finish-card reveal" style={{ display: "grid", gap: 12, alignItems: "center", justifyItems: "center", textAlign: "center", padding: 16 }}>
            <CheckCircle2 size={48} strokeWidth={1.75} style={{ color: "var(--accent)" }} aria-hidden />
            <h3 style={{ margin: 0 }}>Your control panel is ready</h3>
            <div style={{ display: "flex", gap: 8, alignItems: "center", justifyContent: "center", flexWrap: "wrap", maxWidth: "100%" }}>
              <span className="mono" style={{ overflowWrap: "anywhere", wordBreak: "break-all" }}>
                {origin}
              </span>
              <CopyButton text={origin} label="Copy" />
            </div>
            {token ? (
              <div className="token-box" style={{ textAlign: "left", maxWidth: "36rem", width: "100%" }}>
                <strong style={{ fontSize: "0.9em" }}>Sign-in token (save it)</strong>
                <div style={{ display: "flex", gap: 8, alignItems: "center", flexWrap: "wrap" }}>
                  <span className="mono" style={{ overflowWrap: "anywhere", wordBreak: "break-all" }}>{token}</span>
                  <CopyButton text={token} label="Copy token" />
                </div>
                <p className="muted" style={{ fontSize: "0.85em", margin: 0 }}>
                  This tab stays signed in. Other browsers need this token on the Sign in page. It is also saved on the server
                  at <code className="mono">~/.config/omahab/token</code>.
                </p>
              </div>
            ) : null}
            <p className="muted" style={{ fontSize: "0.9em", maxWidth: "36rem", overflowWrap: "anywhere" }}>
              Bundled apps can require domain/HTTPS configuration — check the dashboard for “Not configured” hints. Backups are not configured yet; set them up next.
            </p>
            <button className="button primary" type="button" onClick={() => navigate("/")}>
              Open control panel
            </button>
          </div>
        </Section>
      )}
      </div>
    </div>
  );
}
