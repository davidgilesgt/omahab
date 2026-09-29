import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "../auth";
import { useToast } from "../components/toast";
import { CopyButton } from "../components/copyButton";

// Disaster-recovery restore on the first-run setup checklist (DESIGN
// §5.4, §24.6): walk the admin through the recovery phrase plus the
// backup repository, stage a validated server-config restore through the
// daemon, then trigger the omahab-restore systemd unit which stops
// omahabd, swaps state atomically as root, and restarts omahabd. The
// phrase lives in component state only and is sent once per call over the
// panel-token-authenticated API.

interface RecoverySnapshot {
  id: string;
  time?: string;
  hostname?: string;
  server_config: boolean;
}

interface OperationStatus {
  id: string;
  status: string;
  stage: string;
  error?: string;
}

function authHeaders(token: string): Record<string, string> {
  return token ? { Authorization: `Bearer ${token}` } : {};
}

async function postJSON<T>(path: string, token: string, body: unknown): Promise<T> {
  const res = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json", ...authHeaders(token) },
    body: JSON.stringify(body),
  });
  const data = (await res.json().catch(() => ({}))) as T & { error?: { message?: string } };
  if (!res.ok) throw new Error(data?.error?.message ?? `HTTP ${res.status}`);
  return data;
}

export function RestoreFromBackupControl() {
  const { token: authToken } = useAuth();
  const toast = useToast();
  const queryClient = useQueryClient();
  const [phrase, setPhrase] = useState("");
  const [repoKind, setRepoKind] = useState<"hetzner_storagebox" | "generic">("hetzner_storagebox");
  const [username, setUsername] = useState("");
  const [host, setHost] = useState("");
  const [subPass, setSubPass] = useState("");
  const [location, setLocation] = useState("");
  const [password, setPassword] = useState("");
  const [snapshots, setSnapshots] = useState<RecoverySnapshot[] | null>(null);
  const [chosen, setChosen] = useState<string>("");
  const [operation, setOperation] = useState<OperationStatus | null>(null);
  const [applyStarted, setApplyStarted] = useState(false);

  const token = authToken ?? "";
  const phraseWords = phrase.trim().split(/\s+/).filter(Boolean);
  const phraseOk = phraseWords.length === 24;
  const repoLocation =
    repoKind === "hetzner_storagebox"
      ? `sftp://${username.trim()}@${host.trim()}:23/./omahab`
      : location.trim();
  const repoReady =
    repoKind === "hetzner_storagebox"
      ? !!username.trim() && !!host.trim() && !!subPass
      : !!location.trim() && !!password;

  // Hetzner fields ride along on list and prepare: the daemon uploads the
  // generated backup SSH key once (same flow as setup) so restic can reach
  // the Storage Box from this fresh machine. Nothing is stored.
  const repoBody = (): Record<string, string> => {
    const body: Record<string, string> = { phrase: phrase.trim(), location: repoLocation };
    if (repoKind === "generic") {
      body.password = password;
    } else {
      body.username = username.trim();
      body.host = host.trim();
      body.sub_account_password = subPass;
    }
    return body;
  };

  const listMutation = useMutation({
    mutationFn: async () => {
      const body = repoBody();
      return postJSON<{ snapshots: RecoverySnapshot[] }>("/api/v1/recovery/restore/snapshots", token, body);
    },
    onSuccess: (data) => {
      setSnapshots(data.snapshots ?? []);
      const serverCfg = (data.snapshots ?? []).find((s) => s.server_config);
      if (serverCfg) setChosen(serverCfg.id);
      toast.success(`${data.snapshots?.length ?? 0} snapshot(s) found`);
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Listing failed"),
  });

  const prepareMutation = useMutation({
    mutationFn: async () => {
      const body = repoBody();
      if (chosen) body.snapshot_id = chosen;
      return postJSON<OperationStatus>("/api/v1/recovery/restore", token, body);
    },
    onSuccess: (op) => {
      setOperation(op);
      setSubPass("");
      setPassword("");
      toast.success("Restore staged and validated — applying will restart the server");
      void queryClient.invalidateQueries({ queryKey: ["setup"] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Staging failed"),
  });

  const applyMutation = useMutation({
    mutationFn: async () => {
      if (!operation) throw new Error("No staged restore");
      return postJSON<OperationStatus>(`/api/v1/recovery/restore/${encodeURIComponent(operation.id)}/apply`, token, {
        phrase: phrase.trim(),
      });
    },
    onSuccess: (op) => {
      setApplyStarted(true);
      setOperation(op);
      toast.success("Restore running — the server restarts and this page reconnects");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Apply failed"),
  });

  if (applyStarted) {
    return (
      <div className="form-stack">
        <p>
          Server-config restore is running in the <span className="mono">omahab-restore</span> system unit: omahabd
          stops, the validated state is swapped in atomically (rollback copy kept), and omahabd restarts.
        </p>
        <p className="muted">
          This page will lose the connection briefly. After the restart, reopen the panel — the checklist
          reconverges apps from the restored state; remaining app data restores through the normal per-app
          restore.
        </p>
        <p>
          Progress: <span className="mono">{operation?.stage ?? "unit-started"}</span>
        </p>
      </div>
    );
  }

  if (operation) {
    return (
      <div className="form-stack">
        <p>
          Snapshot staged and validated (<span className="mono">{operation.stage}</span>). Applying stops and
          restarts this server now.
        </p>
        <button className="button danger" type="button" onClick={() => void applyMutation.mutate()} disabled={applyMutation.isPending}>
          {applyMutation.isPending ? "Applying…" : "Apply restore and restart"}
        </button>
        {applyMutation.isError && (
          <p className="inline-error" role="alert">{applyMutation.error instanceof Error ? applyMutation.error.message : "Apply failed"}</p>
        )}
      </div>
    );
  }

  return (
    <div className="form-stack">
      <p className="muted">
        Rebuild this server from an off-site backup: enter the 24-word recovery phrase and the backup
        repository. The system lists snapshots, restores the control plane (accounts, secrets, configuration),
        and restarts. Photos and app data restore per-app afterwards.
      </p>
      <label className="field">
        <span>Recovery phrase (24 words)</span>
        <textarea
          value={phrase}
          onChange={(e) => setPhrase(e.target.value)}
          rows={3}
          placeholder="word1 word2 … word24"
          className="mono"
          autoComplete="off"
          spellCheck={false}
        />
        {phrase.trim() !== "" && !phraseOk && (
          <p className="inline-error" role="alert">Phrase must be exactly 24 words ({phraseWords.length} entered).</p>
        )}
      </label>
      <div style={{ display: "flex", gap: 8, marginBottom: 8, flexWrap: "wrap" }}>
        <button type="button" className={`button ${repoKind === "hetzner_storagebox" ? "primary" : "ghost"}`} onClick={() => setRepoKind("hetzner_storagebox")}>Hetzner Storage Box</button>
        <button type="button" className={`button ${repoKind === "generic" ? "primary" : "ghost"}`} onClick={() => setRepoKind("generic")}>Advanced (restic URL)</button>
      </div>
      {repoKind === "hetzner_storagebox" ? (
        <>
          <label className="field">
            <span>Username (u123456)</span>
            <input value={username} onChange={(e) => setUsername(e.target.value)} placeholder="u123456" autoComplete="off" />
          </label>
          <label className="field">
            <span>Host (u123456.your-storagebox.de)</span>
            <input value={host} onChange={(e) => setHost(e.target.value)} placeholder="u123456.your-storagebox.de" autoComplete="off" />
          </label>
          <label className="field">
            <span>Sub-account password (used once to upload the SSH key)</span>
            <input type="password" value={subPass} onChange={(e) => setSubPass(e.target.value)} autoComplete="new-password" />
          </label>
        </>
      ) : (
        <>
          <label className="field">
            <span>Location (restic URL)</span>
            <input value={location} onChange={(e) => setLocation(e.target.value)} placeholder="/srv/omahab/backups/repos/primary" className="mono" />
          </label>
          <label className="field">
            <span>Repository password</span>
            <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="new-password" />
          </label>
        </>
      )}
      <button
        className="button secondary"
        type="button"
        onClick={() => void listMutation.mutate()}
        disabled={listMutation.isPending || !phraseOk || !repoLocation}
      >
        {listMutation.isPending ? "Listing…" : "List snapshots"}
      </button>
      {listMutation.isError && (
        <p className="inline-error" role="alert">{listMutation.error instanceof Error ? listMutation.error.message : "Listing failed"}</p>
      )}
      {snapshots && (
        snapshots.length === 0 ? (
          <p className="muted">No snapshots found — check the phrase and repository.</p>
        ) : (
          <label className="field">
            <span>Snapshot to restore</span>
            <select value={chosen} onChange={(e) => setChosen(e.target.value)}>
              {snapshots.map((s) => (
                <option key={s.id} value={s.id} disabled={!s.server_config}>
                  {s.id} · {s.time ?? ""}{s.server_config ? "" : " (no server-config)"}
                </option>
              ))}
            </select>
          </label>
        )
      )}
      {snapshots && snapshots.some((s) => s.server_config) && (
        <>
          <button
            className="button primary"
            type="button"
            onClick={() => void prepareMutation.mutate()}
            disabled={prepareMutation.isPending || !chosen}
          >
            {prepareMutation.isPending ? "Staging…" : "Stage validated restore"}
          </button>
          {prepareMutation.isError && (
            <p className="inline-error" role="alert">{prepareMutation.error instanceof Error ? prepareMutation.error.message : "Staging failed"}</p>
          )}
        </>
      )}
      <details>
        <summary className="muted">Prefer SSH? Equivalent commands</summary>
        <div className="code-strip">
          <code>sudo omahab recovery apply --repository {"<repo-url>"}</code>
          <CopyButton text="sudo omahab recovery apply --repository <repo-url>" label="Copy" />
        </div>
        <div className="code-strip">
          <code>sudo omahab recovery apply --repository {"<repo-url>"} --list</code>
          <CopyButton text="sudo omahab recovery apply --repository <repo-url> --list" label="Copy" />
        </div>
      </details>
    </div>
  );
}
