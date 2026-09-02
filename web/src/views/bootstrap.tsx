import { useEffect, useRef, useState, type FormEvent } from "react";
import { PageHeader, Section } from "../components/ui";
import { useToast } from "../components/toast";

const BOOTSTRAP_POLL_MS = 3000;

type Step = "code" | "ssh" | "tailscale" | "handoff";

interface BootstrapResponse {
  ok: boolean;
  error?: string;
}

async function bootstrapFetch(path: string, token: string | null, body?: unknown): Promise<BootstrapResponse & Record<string, unknown>> {
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (token) headers.Authorization = `Bearer ${token}`;
  const resp = await fetch(`/api/bootstrap/${path}`, {
    method: body !== undefined ? "POST" : "GET",
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  const data = (await resp.json().catch(() => ({}))) as Record<string, unknown>;
  if (!resp.ok) {
    throw new Error(typeof data.message === "string" ? data.message : `HTTP ${resp.status}`);
  }
  return data as BootstrapResponse & Record<string, unknown>;
}

export function BootstrapPage() {
  const toast = useToast();
  const [step, setStep] = useState<Step>("code");
  const [token, setToken] = useState<string | null>(null);
  const [code, setCode] = useState("");
  const [githubUser, setGithubUser] = useState("");
  const [pastedKeys, setPastedKeys] = useState("");
  const [authUrl, setAuthUrl] = useState<string | null>(null);
  const [tsRunning, setTsRunning] = useState(false);
  const [tsIp, setTsIp] = useState("");
  const [busy, setBusy] = useState(false);
  const pollRef = useRef<number | null>(null);

  useEffect(() => () => { if (pollRef.current) window.clearInterval(pollRef.current); }, []);

  async function submitCode(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    try {
      const data = await bootstrapFetch("claim", null, { code: code.trim() });
      const t = typeof data.token === "string" ? data.token : null;
      if (!t) throw new Error("no token in response");
      setToken(t);
      setStep("ssh");
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "claim failed");
    } finally {
      setBusy(false);
    }
  }

  async function submitKeys(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    try {
      await bootstrapFetch("ssh-keys", token, {
        github_user: githubUser.trim() || undefined,
        keys: pastedKeys.trim() ? pastedKeys.trim().split("\n").map((k) => k.trim()).filter(Boolean) : [],
      });
      setStep("tailscale");
      await startTailscale();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "failed to install keys");
    } finally {
      setBusy(false);
    }
  }

  function skipKeys() {
    setStep("tailscale");
    void startTailscale();
  }

  async function startTailscale() {
    try {
      const data = await bootstrapFetch("tailscale/up", token);
      const url = typeof data.auth_url === "string" ? data.auth_url : "";
      setAuthUrl(url || null);
      if (pollRef.current) window.clearInterval(pollRef.current);
      pollRef.current = window.setInterval(async () => {
        try {
          const st = await bootstrapFetch("tailscale/status", token);
          const running = st.running === true;
          const ip = typeof st.ip === "string" ? st.ip : "";
          setTsRunning(running);
          setTsIp(ip);
          if (running && ip.startsWith("100.")) {
            if (pollRef.current) window.clearInterval(pollRef.current);
            setStep("handoff");
          }
        } catch {
          // listener may have closed after complete
        }
      }, BOOTSTRAP_POLL_MS);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "tailscale up failed");
    }
  }

  async function complete() {
    try {
      await bootstrapFetch("complete", token);
    } catch {
      /* listener closes on completion */
    }
    const ip = tsIp || window.location.hostname;
    window.location.href = `http://${ip}:8484/#token=${encodeURIComponent(token ?? "")}`;
  }

  return (
    <div className="page">
      <PageHeader
        eyebrow="First boot"
        title="Omahab setup"
        description="Claim this server with the one-time code shown on its console, then enroll it in your tailnet."
      />
      {step === "code" && (
        <Section title="1 · Enter the one-time code" description="The code is displayed on the server's console (tty1) and rotates on repeated wrong attempts.">
          <form className="form-grid" onSubmit={submitCode}>
            <label className="field">
              <span>One-time code</span>
              <input value={code} onChange={(e) => setCode(e.target.value)} placeholder="e.g. 7gc3x9k2mq" autoFocus className="mono" autoComplete="off" />
            </label>
            <div className="form-actions">
              <button className="button primary" type="submit" disabled={busy || code.trim().length < 4}>{busy ? "Claiming…" : "Claim"}</button>
            </div>
          </form>
        </Section>
      )}
      {step === "ssh" && (
        <Section title="2 · Administrator SSH keys" description="Import keys from GitHub or paste public keys. Skipping is allowed — console access remains the recovery path.">
          <form className="form-grid" onSubmit={submitKeys}>
            <label className="field">
              <span>GitHub username</span>
              <input value={githubUser} onChange={(e) => setGithubUser(e.target.value)} placeholder="your-github-username" autoComplete="off" />
            </label>
            <label className="field">
              <span>…or paste public keys (one per line)</span>
              <textarea value={pastedKeys} onChange={(e) => setPastedKeys(e.target.value)} rows={4} placeholder="ssh-ed25519 AAAA…" className="mono" />
            </label>
            <div className="form-actions">
              <button className="button ghost" type="button" onClick={skipKeys} disabled={busy}>Skip</button>
              <button className="button primary" type="submit" disabled={busy || (!githubUser.trim() && !pastedKeys.trim())}>{busy ? "Installing…" : "Install keys"}</button>
            </div>
          </form>
        </Section>
      )}
      {step === "tailscale" && (
        <Section title="3 · Tailscale" description="Approve this server from any device signed into your tailnet. The dashboard is reachable only over the tailnet.">
          {authUrl ? (
            <div className="callout">
              <p>Open this URL to authorize the server:</p>
              <p><a href={authUrl} target="_blank" rel="noreferrer" className="mono">{authUrl}</a></p>
            </div>
          ) : (
            <p className="muted">Starting tailscale…</p>
          )}
          <p className="muted">Status: {tsRunning ? "running" : "waiting for approval"}{tsIp ? ` · ${tsIp}` : ""}</p>
        </Section>
      )}
      {step === "handoff" && (
        <Section title="Setup complete" description="The rest of enrollment (domain, Cloudflare, recovery key, storage, backups) happens on the authenticated dashboard over Tailscale — secrets never transit this LAN page.">
          <div className="callout">
            <p>Your dashboard: <span className="mono">http://{tsIp}:8484</span></p>
          </div>
          <div className="form-actions">
            <button className="button primary" type="button" onClick={complete}>Open dashboard</button>
          </div>
        </Section>
      )}
    </div>
  );
}
