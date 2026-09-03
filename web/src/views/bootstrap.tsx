import { useEffect, useRef, useState, type FormEvent } from "react";
import { PageHeader, Section } from "../components/ui";
import { useToast } from "../components/toast";

const BOOTSTRAP_POLL_MS = 3000;

type Step = "code" | "mode" | "ssh" | "tailscale" | "handoff" | "restore" | "restore_running";

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
  // Restore state
  const [restoreKind, setRestoreKind] = useState<"hetzner_storagebox" | "generic">("hetzner_storagebox");
  const [restoreUsername, setRestoreUsername] = useState("");
  const [restoreHost, setRestoreHost] = useState("");
  const [restoreSubPass, setRestoreSubPass] = useState("");
  const [restoreLocation, setRestoreLocation] = useState("");
  const [restorePhrase, setRestorePhrase] = useState("");
  const [restoreSnapshots, setRestoreSnapshots] = useState<Array<{ id: string; time: string; hostname: string }>>([]);
  const [restoreSelected, setRestoreSelected] = useState<string | null>(null);
  const [restoreLogs, setRestoreLogs] = useState<string[]>([]);

  useEffect(() => () => { if (pollRef.current) window.clearInterval(pollRef.current); }, []);

  async function submitCode(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    try {
      const data = await bootstrapFetch("claim", null, { code: code.trim() });
      const t = typeof data.token === "string" ? data.token : null;
      if (!t) throw new Error("no token in response");
      setToken(t);
      setStep("mode");
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

  async function submitRestoreConnect(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    try {
      const body: Record<string, string> = { phrase: restorePhrase.trim() };
      if (restoreKind === "hetzner_storagebox") {
        body.kind = "hetzner_storagebox";
        body.username = restoreUsername.trim();
        body.host = restoreHost.trim();
        body.sub_account_password = restoreSubPass;
      } else {
        body.location = restoreLocation.trim();
      }
      const data = await bootstrapFetch("restore/connect", token, body);
      const snaps = (data.snapshots as Array<{ id: string; time: string; hostname: string }>) ?? [];
      setRestoreSnapshots(snaps);
      if (snaps.length === 0) toast.error("No snapshots found");
      else if (snaps.length === 1) setRestoreSelected(snaps[0].id);
      toast.success(`Found ${snaps.length} snapshot(s)`);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "connect failed");
    } finally {
      setBusy(false);
    }
  }

  async function submitRestoreRun() {
    if (!restoreSelected) {
      toast.error("Select a snapshot");
      return;
    }
    setBusy(true);
    setRestoreLogs([]);
    try {
      await bootstrapFetch("restore/run", token, { snapshot_id: restoreSelected });
      setStep("restore_running");
      const headers: Record<string, string> = {};
      if (token) headers.Authorization = `Bearer ${token}`;
      const resp = await fetch(`/api/bootstrap/restore/events`, { headers });
      if (!resp.ok || !resp.body) throw new Error(`events stream failed ${resp.status}`);
      const reader = resp.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        const parts = buffer.split("\n\n");
        buffer = parts.pop() ?? "";
        for (const part of parts) {
          const line = part.trim();
          if (line.startsWith("data:")) {
            const jsonStr = line.slice(5).trim();
            try {
              const ev = JSON.parse(jsonStr) as { stage: string; message: string; done?: boolean; error?: string };
              setRestoreLogs((prev) => [...prev, `${ev.stage}: ${ev.message}`]);
              if (ev.done) {
                if (ev.error) toast.error(ev.error);
                else toast.success("Restore complete, restarting");
                setTimeout(() => setStep("handoff"), 1200);
              }
            } catch {}
          }
        }
      }
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "restore failed");
    } finally {
      setBusy(false);
    }
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
      {step === "mode" && (
        <Section title="2 · Choose setup mode" description="Set up a new server or restore from a Hetzner Storage Box backup.">
          <div className="form-grid" style={{ gap: 12 }}>
            <button className="button primary" type="button" onClick={() => setStep("ssh")} disabled={busy}>Set up a new server</button>
            <button className="button secondary" type="button" onClick={() => setStep("restore")} disabled={busy}>Restore from backup</button>
            <p className="muted" style={{ fontSize: "0.9em" }}>Restore collects Hetzner Storage Box username + host (or generic restic location), sub-account password once, and the 24-word recovery phrase. The phrase alone opens the repository.</p>
          </div>
        </Section>
      )}
      {step === "restore" && (
        <Section title="Restore from backup" description="Enter Hetzner Storage Box credentials (recommended, ~€4/mo) or a generic restic URL (Advanced), plus the 24-word recovery phrase.">
          <form className="form-grid" onSubmit={submitRestoreConnect} style={{ gap: 12 }}>
            <div style={{ display: "flex", gap: 8 }}>
              <button type="button" className={`button ${restoreKind === "hetzner_storagebox" ? "primary" : "ghost"}`} onClick={() => setRestoreKind("hetzner_storagebox")}>Hetzner Storage Box</button>
              <button type="button" className={`button ${restoreKind === "generic" ? "primary" : "ghost"}`} onClick={() => setRestoreKind("generic")}>Advanced (restic URL)</button>
            </div>
            {restoreKind === "hetzner_storagebox" ? (
              <>
                <label className="field">
                  <span>Username (u123456)</span>
                  <input value={restoreUsername} onChange={(e) => setRestoreUsername(e.target.value)} placeholder="u123456" autoComplete="off" />
                </label>
                <label className="field">
                  <span>Host (u123456.your-storagebox.de)</span>
                  <input value={restoreHost} onChange={(e) => setRestoreHost(e.target.value)} placeholder="u123456.your-storagebox.de" autoComplete="off" />
                </label>
                <label className="field">
                  <span>Sub-account password (used once to upload SSH key)</span>
                  <input type="password" value={restoreSubPass} onChange={(e) => setRestoreSubPass(e.target.value)} autoComplete="off" />
                </label>
                <p className="muted" style={{ fontSize: "0.85em" }}>Hetzner Storage Box (recommended, ~€4/mo). Create a sub-account with SSH enabled; enter its username and password once.</p>
              </>
            ) : (
              <label className="field">
                <span>Repository location (restic URL)</span>
                <input value={restoreLocation} onChange={(e) => setRestoreLocation(e.target.value)} placeholder="sftp:user@host:restic-repo" className="mono" />
              </label>
            )}
            <label className="field">
              <span>Recovery phrase (24 words)</span>
              <textarea value={restorePhrase} onChange={(e) => setRestorePhrase(e.target.value)} rows={3} placeholder="24 words separated by spaces" className="mono" autoComplete="off" />
            </label>
            <div className="form-actions">
              <button className="button ghost" type="button" onClick={() => setStep("mode")} disabled={busy}>Back</button>
              <button className="button primary" type="submit" disabled={busy || restorePhrase.trim().split(/\s+/).length < 24}>{busy ? "Connecting…" : "Connect and list snapshots"}</button>
            </div>
            {restoreSnapshots.length > 0 && (
              <div style={{ border: "1px solid var(--border)", borderRadius: 6, padding: 12 }}>
                <p><strong>Available snapshots (latest 10):</strong></p>
                <ul style={{ listStyle: "none", padding: 0, display: "grid", gap: 6 }}>
                  {restoreSnapshots.map((s) => (
                    <li key={s.id} style={{ display: "flex", gap: 8, alignItems: "center", padding: 6, border: restoreSelected === s.id ? "2px solid var(--primary)" : "1px solid var(--border)", borderRadius: 4 }}>
                      <input type="radio" name="snapshot" checked={restoreSelected === s.id} onChange={() => setRestoreSelected(s.id)} />
                      <span className="mono" style={{ fontSize: "0.85em" }}>{s.id.slice(0, 8)}</span>
                      <span style={{ opacity: 0.8 }}>{s.hostname}</span>
                      <span style={{ opacity: 0.6, fontSize: "0.85em" }}>{new Date(s.time).toLocaleString()}</span>
                    </li>
                  ))}
                </ul>
                <div className="form-actions" style={{ marginTop: 12 }}>
                  <button className="button primary" type="button" onClick={submitRestoreRun} disabled={busy || !restoreSelected}>{busy ? "Starting…" : "Restore selected snapshot"}</button>
                </div>
              </div>
            )}
          </form>
        </Section>
      )}
      {step === "restore_running" && (
        <Section title="Restoring…" description="Restoring snapshot to / (includes /var/lib/omahab, /var/lib/tailscale, app data). Never writes under /nix or /etc.">
          <div style={{ background: "var(--surface-muted)", padding: 12, borderRadius: 6, maxHeight: 300, overflowY: "auto", fontFamily: "monospace", fontSize: "0.85em" }}>
            {restoreLogs.length === 0 ? <p className="muted">Starting…</p> : restoreLogs.map((l, i) => <div key={i}>{l}</div>)}
          </div>
          <p className="muted" style={{ marginTop: 8 }}>After restore the Tailscale identity (/var/lib/tailscale) is kept; if the coordination server rejects it, you will be returned to the Tailscale step.</p>
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
