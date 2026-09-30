import { useMemo, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "../auth";
import { useToast } from "../components/toast";
import { CopyButton } from "../components/copyButton";
import type { RecoveryInstance, RecoverySnapshot } from "../api/types";

// The FIRST setup step (DESIGN §5.3/§5.4): "Do you have a recovery phrase
// from an earlier Omahab server?" NO walks the new-server path (phrase
// ritual lives in setup.tsx; this control only configures the repository);
// YES walks instance discovery -> snapshot pick -> full or scoped restore
// through the staged omahab-restore path. The phrase lives in component
// state only and is sent once per call over the panel-token API.
//
// Security: before Tailscale the panel is plain HTTP on the LAN, so the
// Hetzner sub-account password crosses the LAN in cleartext. The SSH
// alternative is shown alongside. Locations are never built client-side:
// the server returns the exact resolvable location for each instance.

interface OperationStatus {
  id: string;
  status: string;
  stage: string;
  error?: string;
  preview?: string;
}

type RestoreChoice = "everything" | "some";

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

export function RestoreFromBackupControl({ onStartNew }: { onStartNew: () => void }) {
  const { client, token: authToken } = useAuth();
  const toast = useToast();
  const queryClient = useQueryClient();
  const [phrase, setPhrase] = useState("");
  const [repoKind, setRepoKind] = useState<"hetzner_storagebox" | "generic">("hetzner_storagebox");
  const [username, setUsername] = useState("");
  const [host, setHost] = useState("");
  const [subPass, setSubPass] = useState("");
  const [location, setLocation] = useState("");
  const [password, setPassword] = useState("");
  const [instances, setInstances] = useState<RecoveryInstance[] | null>(null);
  const [instanceLocation, setInstanceLocation] = useState("");
  const [snapshots, setSnapshots] = useState<RecoverySnapshot[] | null>(null);
  const [chosen, setChosen] = useState<string>("");
  const [choice, setChoice] = useState<RestoreChoice>("everything");
  const [appPicks, setAppPicks] = useState<string[]>([]);
  const [operation, setOperation] = useState<OperationStatus | null>(null);
  const [applyStarted, setApplyStarted] = useState(false);

  const token = authToken ?? "";
  const phraseWords = phrase.trim().split(/\s+/).filter(Boolean);
  const phraseOk = phraseWords.length === 24;
  const repoReady =
    repoKind === "hetzner_storagebox"
      ? !!username.trim() && !!host.trim()
      : !!location.trim() && !!password;

  // The chosen snapshot drives the app-scope picker; server-config is always
  // restored and never appears as an optional checkbox.
  const chosenSnapshot = useMemo(
    () => snapshots?.find((s) => s.id === chosen) ?? null,
    [snapshots, chosen],
  );
  const appScopes = useMemo(
    () => (chosenSnapshot?.scopes ?? []).filter((s) => s.startsWith("app:")),
    [chosenSnapshot],
  );

  const instanceBody = () =>
    repoKind === "hetzner_storagebox"
      ? { phrase: phrase.trim(), username: username.trim(), host: host.trim(), sub_account_password: subPass }
      : { phrase: phrase.trim(), location: location.trim(), password };

  // List earlier instances under ./omahab on the Storage Box (or the
  // generic repo's parent). The server returns the exact location per
  // instance; nothing is assembled client-side.
  const instancesMutation = useMutation({
    mutationFn: async () => client.listRecoveryInstances(instanceBody()),
    onSuccess: (data) => {
      setInstances(data.instances ?? []);
      const usable = (data.instances ?? []).find((i) => !i.locked);
      if (usable) setInstanceLocation(usable.location);
      toast.success(`${data.instances?.length ?? 0} earlier server(s) found`);
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Listing failed"),
  });

  const snapshotsMutation = useMutation({
    mutationFn: async () => {
      if (!instanceLocation) throw new Error("Pick an earlier server first");
      const body: Record<string, unknown> = { phrase: phrase.trim(), location: instanceLocation };
      if (repoKind === "generic") body.password = password;
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
      const body: Record<string, unknown> = {
        phrase: phrase.trim(),
        location: instanceLocation,
        snapshot_id: chosen,
      };
      if (repoKind === "generic") body.password = password;
      if (choice === "some") {
        body.scopes = ["server-config", ...appPicks];
      } else {
        body.include_apps = true;
      }
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

  // "Start new instead": record the choice server-side so the checklist
  // stops offering a restore, then hand the operator to the new-server
  // path (phrase + repository) inside the same first step.
  const skipMutation = useMutation({
    mutationFn: () => client.skipRecoveryRestore(),
    onSuccess: () => {
      toast.success("Starting fresh — set up the recovery phrase and backups below");
      setInstances(null);
      setSnapshots(null);
      setInstanceLocation("");
      void queryClient.invalidateQueries({ queryKey: ["setup"] });
      onStartNew();
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Could not record the choice"),
  });

  if (applyStarted) {
    return (
      <div className="form-stack">
        <p>
          Restore is running in the <span className="mono">omahab-restore</span> system unit: omahabd
          stops, the validated state is swapped in atomically (rollback copy kept), and omahabd restarts.
        </p>
        <p className="muted">
          This page will lose the connection briefly. After the restart, reopen the panel — the
          checklist reconverges apps from the restored state; remaining app data restores through the
          normal per-app restore.
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
        {operation.preview && (
          <p className="inline-error" role="alert">{operation.preview}</p>
        )}
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
        Rebuild an earlier Omahab server from its off-site backup: enter the 24-word recovery phrase and
        the backup account, pick the server instance and snapshot, then restore. Server configuration
        (accounts, secrets, settings) always restores; app data can follow in full or per-app.
      </p>
      <div className="setup-notice" role="note">
        <strong>Heads-up before Tailscale is up:</strong> this panel travels over plain HTTP on your LAN,
        so the Hetzner sub-account password you type here crosses the LAN unencrypted. If that matters,
        SSH in and run the same flow on the server instead (commands below).
      </div>
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
        onClick={() => void instancesMutation.mutate()}
        disabled={instancesMutation.isPending || !phraseOk || !repoReady}
      >
        {instancesMutation.isPending ? "Finding earlier servers…" : "Find earlier servers"}
      </button>
      {instancesMutation.isError && (
        <p className="inline-error" role="alert">{instancesMutation.error instanceof Error ? instancesMutation.error.message : "Listing failed"}</p>
      )}
      {instances && (
        instances.length === 0 ? (
          <p className="muted">No earlier servers found — check the phrase and account, or start new below.</p>
        ) : (
          <label className="field">
            <span>Earlier server instance</span>
            <select value={instanceLocation} onChange={(e) => { setInstanceLocation(e.target.value); setSnapshots(null); }}>
              <option value="">Pick an instance…</option>
              {instances.map((i) => (
                <option key={i.instance_id} value={i.location} disabled={i.locked}>
                  {i.instance_id}{i.domain ? ` (${i.domain})` : ""}{i.latest_snapshot_time ? ` · ${i.latest_snapshot_time}` : ""}{i.locked ? " · locked (wrong phrase for this one)" : ""}
                </option>
              ))}
            </select>
          </label>
        )
      )}
      {instanceLocation && !snapshots && (
        <button
          className="button secondary"
          type="button"
          onClick={() => void snapshotsMutation.mutate()}
          disabled={snapshotsMutation.isPending || !phraseOk}
        >
          {snapshotsMutation.isPending ? "Listing…" : "List snapshots"}
        </button>
      )}
      {snapshotsMutation.isError && (
        <p className="inline-error" role="alert">{snapshotsMutation.error instanceof Error ? snapshotsMutation.error.message : "Listing failed"}</p>
      )}
      {snapshots && (
        snapshots.length === 0 ? (
          <p className="muted">No snapshots found in this instance — pick another or start new.</p>
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
      {chosenSnapshot && (
        <div className="form-stack">
          <span>What to restore</span>
          <label style={{ display: "flex", gap: 8, alignItems: "center" }}>
            <input type="radio" name="restore-choice" checked={choice === "everything"} onChange={() => setChoice("everything")} />
            Restore everything — server configuration plus all app data from this snapshot
          </label>
          {appScopes.length > 0 && (
            <label style={{ display: "flex", gap: 8, alignItems: "center" }}>
              <input type="radio" name="restore-choice" checked={choice === "some"} onChange={() => { setChoice("some"); setAppPicks([]); }} />
              Restore some apps
            </label>
          )}
          {choice === "some" && (
            <div className="form-stack" style={{ paddingLeft: 8 }}>
              <p className="muted" style={{ margin: 0 }}>
                Server configuration is always included — app data needs it (secrets, accounts, routes).
              </p>
              {appScopes.map((scope) => {
                const id = scope.slice("app:".length);
                return (
                  <label key={scope} style={{ display: "flex", gap: 8, alignItems: "center" }}>
                    <input
                      type="checkbox"
                      checked={appPicks.includes(scope)}
                      onChange={(e) =>
                        setAppPicks(e.target.checked ? [...appPicks, scope] : appPicks.filter((s) => s !== scope))
                      }
                    />
                    {id}
                  </label>
                );
              })}
            </div>
          )}
          <button
            className="button primary"
            type="button"
            onClick={() => void prepareMutation.mutate()}
            disabled={prepareMutation.isPending || !chosen || (choice === "some" && appScopes.length === 0)}
          >
            {prepareMutation.isPending ? "Staging…" : "Stage validated restore"}
          </button>
          {prepareMutation.isError && (
            <p className="inline-error" role="alert">{prepareMutation.error instanceof Error ? prepareMutation.error.message : "Staging failed"}</p>
          )}
        </div>
      )}
      <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
        <button
          className="button ghost"
          type="button"
          onClick={() => void skipMutation.mutate()}
          disabled={skipMutation.isPending}
        >
          {skipMutation.isPending ? "Starting…" : "Start new instead"}
        </button>
      </div>
      {skipMutation.isError && (
        <p className="inline-error" role="alert">{skipMutation.error instanceof Error ? skipMutation.error.message : "Could not record the choice"}</p>
      )}
      <details>
        <summary className="muted">Prefer SSH? Equivalent commands</summary>
        <div className="code-strip">
          <code>sudo omahab recovery apply --repository {"<repo-url>"} --list</code>
          <CopyButton text="sudo omahab recovery apply --repository <repo-url> --list" label="Copy" />
        </div>
        <div className="code-strip">
          <code>sudo omahab recovery apply --repository {"<repo-url>"}</code>
          <CopyButton text="sudo omahab recovery apply --repository <repo-url>" label="Copy" />
        </div>
        <p className="muted" style={{ fontSize: "0.85rem" }}>
          The recovery-phrase prompt reads from the terminal, never argv, and the repository password is
          derived exactly as during setup.
        </p>
      </details>
    </div>
  );
}
