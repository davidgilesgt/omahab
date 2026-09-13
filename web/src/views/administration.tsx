// P1-3 integrator note: No dedicated knowledge/assistant settings view exists in web/src/views.
// The required UI (three semantic-index options: Best English / Best worldwide / Full-text only,
// pinned-model metadata from pinned_models.json, and summarization-consent dialog with provider
// and informed choice) should be implemented as a new route (e.g., /knowledge or /ai/settings)
// or integrated into the Sync folders view once API routes /api/v1/knowledge/* are available.
// Workers/embedding/pinned_models.json.example contains the model metadata shape.
import { useEffect, useRef, useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "../auth";
import type { RecoverySession, SSHKey, User, Workspace } from "../api/types";
import { EmptyState, ErrorState, formatDate, LoadingState, PageHeader, Section, StatusPill } from "../components/ui";
import { useToast } from "../components/toast";
import { CopyButton } from "../components/copyButton";

const GROUP_OPTIONS = ["admins", "members", "guests"] as const;

function OperationError({ error }: { error: unknown }) {
  return error ? <p className="inline-error" role="alert">{error instanceof Error ? error.message : "The operation failed."}</p> : null;
}
function welcomeUrlFromEnrollment(enrollmentUrl: string): string | null {
  try {
    const u = new URL(enrollmentUrl);
    let token = u.searchParams.get("token") || u.searchParams.get("code");
    if (!token) {
      const parts = u.pathname.split("/").filter(Boolean);
      token = parts[parts.length - 1] || null;
    }
    if (!token) return null;
    const host = u.hostname;
    const hostParts = host.split(".");
    const domain = hostParts.length >= 2 ? hostParts.slice(1).join(".") : host;
    let homeHost = "";
    try {
      const cur = window.location.hostname;
      if (cur.startsWith("home.")) homeHost = cur;
      else if (cur.includes(".home.")) homeHost = `home.${cur.split(".home.")[1]}`;
      else if (domain) homeHost = `home.${domain}`;
    } catch {}
    const scheme = typeof window !== "undefined" ? window.location.protocol : "https:";
    if (!homeHost) return null;
    return `${scheme}//${homeHost}/welcome/${encodeURIComponent(token)}`;
  } catch {
    return null;
  }
}


function DestructiveConfirm({
  title,
  description,
  confirmValue,
  confirmLabel,
  onConfirm,
  onClose,
  pending,
  error,
}: {
  title: string;
  description: string;
  confirmValue: string;
  confirmLabel: string;
  onConfirm: () => void;
  onClose: () => void;
  pending?: boolean;
  error?: unknown;
}) {
  const [input, setInput] = useState("");
  const dialogRef = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const dialog = dialogRef.current;
    const returnFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    if (dialog && !dialog.open) dialog.showModal();
    return () => {
      if (dialog?.open) dialog.close();
      returnFocus?.focus();
    };
  }, []);
  const confirmed = input === confirmValue;
  return (
    <dialog ref={dialogRef} className="modal" aria-labelledby="confirm-title" onCancel={(event) => { event.preventDefault(); onClose(); }}>
      <header><div><p className="eyebrow">Confirm</p><h2 id="confirm-title">{title}</h2></div><button type="button" className="icon-button" onClick={onClose} aria-label="Close">×</button></header>
      <div className="form-stack">
        <p>{description}</p>
        <div className="danger-zone">
          <strong>This action cannot be undone.</strong>
          <p>Type <span className="mono">{confirmValue}</span> to continue.</p>
          <label>{confirmLabel} <span className="mono">{confirmValue}</span>
            <input value={input} onChange={(event) => setInput(event.currentTarget.value)} autoComplete="off" spellCheck={false} />
          </label>
        </div>
        {error ? <p className="inline-error" role="alert">{error instanceof Error ? error.message : "The operation failed."}</p> : null}
        <div className="modal-actions">
          <button type="button" className="button secondary" onClick={onClose}>Cancel</button>
          <button type="button" className="button danger" disabled={!confirmed || pending} onClick={onConfirm}>{pending ? "Working…" : "Confirm"}</button>
        </div>
      </div>
    </dialog>
  );
}

function GroupMultiSelect({
  values,
  onChange,
  idPrefix,
}: {
  values: string[];
  onChange: (next: string[]) => void;
  idPrefix: string;
}) {
  function toggle(group: string) {
    if (values.includes(group)) onChange(values.filter((v) => v !== group));
    else onChange([...values, group]);
  }
  return (
    <fieldset className="form-stack" style={{ border: 0, padding: 0, margin: 0 }}>
      <legend className="muted" style={{ fontSize: "0.875rem" }}>Groups</legend>
      <div className="row-actions" style={{ justifyContent: "flex-start" }}>
        {GROUP_OPTIONS.map((group) => (
          <label key={group} className="check-row" style={{ gap: "0.5rem" }}>
            <input
              type="checkbox"
              id={`${idPrefix}-${group}`}
              checked={values.includes(group)}
              onChange={() => toggle(group)}
            />
            <span>{group}</span>
          </label>
        ))}
      </div>
      <small className="muted">API already accepts groups on create/update. Static options are admins/members/guests.</small>
    </fieldset>
  );
}

export function SyncFoldersPage() {
  const { client } = useAuth();
  const queryClient = useQueryClient();
  const toast = useToast();
  const query = useQuery({ queryKey: ["sync-folders"], queryFn: client.syncFolders });
  const create = useMutation({
    mutationFn: client.createSyncFolder,
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["sync-folders"] });
      toast.success("Folder added");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Could not add folder"),
  });
  const update = useMutation({
    mutationFn: ({ id, share }: { id: string; share: boolean }) => client.updateSyncFolder(id, { share_with_ai: share }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["sync-folders"] });
      toast.success("Folder updated");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Update failed"),
  });
  const folders = query.data ?? [];

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const formElement = event.currentTarget;
    const form = new FormData(formElement);
    create.mutate(
      { name: String(form.get("name") ?? "").trim(), server_path: String(form.get("path") ?? "").trim(), share_with_ai: form.get("share") === "on" },
      { onSuccess: () => formElement.reset() },
    );
  }

  return (
    <div className="page">
      <PageHeader eyebrow="Knowledge" title="Sync folders" description="Manage server-side Syncthing folders and their explicit AI reading permission." />
      <div className="split-grid wide-primary">
        <Section title="Folders" description="Sharing with AI permits the default assistant to list, search, and read this folder.">
          {query.isLoading ? <LoadingState label="Loading folders" /> : query.isError ? <ErrorState error={query.error} retry={() => void query.refetch()} /> : !folders.length ? <EmptyState title="No synchronized folders" description="Add a server folder to begin syncing it with trusted devices." /> : (
            <div className="compact-list">{folders.map((folder) => <div key={folder.id}><div><strong>{folder.name}</strong><span className="mono">{folder.server_path} <CopyButton text={folder.server_path} label="Copy" /></span></div><StatusPill value={folder.health} /><label className="switch"><input type="checkbox" checked={folder.share_with_ai} disabled={update.isPending} onChange={(event) => update.mutate({ id: folder.id, share: event.currentTarget.checked })} /><span>Share with AI</span></label></div>)}</div>
          )}
          <OperationError error={update.error} />
        </Section>
        <Section title="Add folder" description="The server path must already exist and be writable by the sync service.">
          <form className="form-stack" onSubmit={submit}>
            <label>Name<input name="name" required /></label>
            <label>Server path<input name="path" className="mono" required /></label>
            <label className="check-row"><input name="share" type="checkbox" /><span><strong>Share with AI</strong><small>The default assistant may read this folder.</small></span></label>
            <button className="button primary" type="submit" disabled={create.isPending}>{create.isPending ? "Adding…" : "Add folder"}</button>
            <OperationError error={create.error} />
          </form>
        </Section>
      </div>
    </div>
  );
}

export function WorkspacesPage() {
  const { client } = useAuth();
  const queryClient = useQueryClient();
  const toast = useToast();
  const workspaces = useQuery({ queryKey: ["workspaces"], queryFn: client.workspaces });
  const projects = useQuery({ queryKey: ["projects"], queryFn: client.projects });
  const create = useMutation({
    mutationFn: client.createWorkspace,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["workspaces"] });
      toast.success("Workspace created");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Could not create workspace"),
  });
  const stop = useMutation({
    mutationFn: client.stopWorkspace,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["workspaces"] });
      toast.success("Workspace stopped");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Could not stop workspace"),
  });
  const workspaceItems = workspaces.data ?? [];
  const projectItems = projects.data ?? [];
  const [stopConfirm, setStopConfirm] = useState<Workspace | null>(null);

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    create.mutate({ project_id: String(form.get("project")), branch: String(form.get("branch") ?? "master").trim(), agent: String(form.get("agent")) });
  }

  return (
    <div className="page">
      <PageHeader eyebrow="Remote development" title="Workspaces" description="Isolated, expiring project environments without production secrets or Docker socket access." />
      <div className="split-grid wide-primary">
        <Section title="Active and recent">
          {workspaces.isLoading ? <LoadingState label="Loading workspaces" /> : workspaces.isError ? <ErrorState error={workspaces.error} retry={() => void workspaces.refetch()} /> : !workspaceItems.length ? <EmptyState title="No workspaces" description="Create one for a project when you need an isolated coding environment." /> : (
            <div className="resource-list inset">{workspaceItems.map((workspace) => {
              const project = projectItems.find((item) => item.id === workspace.project_id);
              return <article className="resource-row" key={workspace.id}><div><div className="resource-title"><strong>{project?.name ?? workspace.project_id}</strong><StatusPill value={workspace.status} /></div><p><span className="mono">{workspace.branch}</span> <CopyButton text={workspace.branch} label="Copy" /> · {workspace.agent}</p><small>Last active {formatDate(workspace.last_active_at)}{workspace.expires_at ? ` · expires ${formatDate(workspace.expires_at)}` : ""}</small></div>{workspace.status !== "stopped" && <button className="button secondary" type="button" disabled={stop.isPending} onClick={() => setStopConfirm(workspace)}>Stop</button>}</article>})}</div>
          )}
          <OperationError error={stop.error} />
        </Section>
        <Section title="New workspace" description="Uses the selected project repository and development container.">
          {projects.isLoading ? <LoadingState label="Loading projects" /> : projects.isError ? <ErrorState error={projects.error} /> : (
            <form className="form-stack" onSubmit={submit}>
              <label>Project<select name="project" required disabled={!projectItems.length}>{projectItems.map((project) => <option key={project.id} value={project.id}>{project.name}</option>)}</select></label>
              <label>Branch<input name="branch" defaultValue="master" required /></label>
              <label>Agent<select name="agent" defaultValue="omp"><option value="omp">OMP</option><option value="codex">Codex</option></select></label>
              <button className="button primary" type="submit" disabled={create.isPending || !projectItems.length}>{create.isPending ? "Creating…" : "Create workspace"}</button>
              {!projectItems.length && <p className="muted">Create a project before starting a workspace.</p>}
              <OperationError error={create.error} />
            </form>
          )}
        </Section>
      </div>
      {stopConfirm && (
        <DestructiveConfirm
          title="Stop workspace"
          description={`Stop workspace ${stopConfirm.branch} (${stopConfirm.agent})? Any unsaved work in the isolated environment will be lost.`}
          confirmValue={stopConfirm.branch}
          confirmLabel="Type the branch name"
          onClose={() => setStopConfirm(null)}
          onConfirm={() => stop.mutate(stopConfirm.id)}
          pending={stop.isPending}
          error={stop.error}
        />
      )}
    </div>
  );
}

export function PeoplePage() {
  const { client, token } = useAuth();
  const queryClient = useQueryClient();
  const toast = useToast();
  const query = useQuery({ queryKey: ["users"], queryFn: client.users });
  const [recovery, setRecovery] = useState<RecoverySession | null>(null);
  const [createGroups, setCreateGroups] = useState<string[]>(["members"]);
  const [editingUser, setEditingUser] = useState<User | null>(null);
  const [editGroups, setEditGroups] = useState<string[]>([]);
  const [toggleTarget, setToggleTarget] = useState<User | null>(null);
  const [enrollmentUrl, setEnrollmentUrl] = useState<string | null>(null);
  const [enrollmentExpires, setEnrollmentExpires] = useState<string | null>(null);
  const create = useMutation({
    mutationFn: (input: { name: string; email: string; groups: string[] }) =>
      (client.createUser as unknown as (i: { name: string; email: string; groups: string[] }) => Promise<User>)(input),
    onSuccess: (data) => {
      queryClient.invalidateQueries({ queryKey: ["users"] });
      if (data.enrollment_url) {
        setEnrollmentUrl(data.enrollment_url);
        setEnrollmentExpires(data.enrollment_expires_at ?? null);
      }
      toast.success("Invite sent");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Invite failed"),
  });
  const enrollment = useMutation({
    mutationFn: (id: string) => client.issueEnrollment(id),
    onSuccess: (data) => {
      if (data.enrollment_url) {
        setEnrollmentUrl(data.enrollment_url);
        setEnrollmentExpires(data.enrollment_expires_at ?? null);
        toast.success("Enrollment link ready");
      }
      void queryClient.invalidateQueries({ queryKey: ["users"] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Enrollment failed"),
  });
  const update = useMutation({
    mutationFn: ({ id, disabled }: { id: string; disabled: boolean }) => client.setUserDisabled(id, disabled),
    onSuccess: (_data, vars) => {
      queryClient.invalidateQueries({ queryKey: ["users"] });
      toast.success(vars.disabled ? "User disabled" : "User enabled");
      setToggleTarget(null);
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Could not update user"),
  });
  const recover = useMutation({
    mutationFn: client.beginRecovery,
    onSuccess: (data) => {
      setRecovery(data);
      toast.success("Recovery session created");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Recovery failed"),
  });
  const updateGroups = useMutation({
    mutationFn: async ({ id, groups }: { id: string; groups: string[] }) => {
      const url = `/api/v1/users/${encodeURIComponent(id)}`;
      const headers: Record<string, string> = { "Content-Type": "application/json" };
      if (token) headers.Authorization = `Bearer ${token}`;
      const res = await fetch(url, { method: "PATCH", headers, body: JSON.stringify({ groups }) });
      if (!res.ok) {
        let message = res.statusText;
        try {
          const data = (await res.json()) as { error?: { message?: string } };
          if (data.error?.message) message = data.error.message;
        } catch {
          // ignore parse failure
        }
        throw new Error(message);
      }
      return (await res.json()) as User;
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["users"] });
      toast.success("Groups updated");
      setEditingUser(null);
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Could not update groups"),
  });
  const users = query.data ?? [];

  // Server SSH access - independent of Pocket ID user queries
  const sshQuery = useQuery({ queryKey: ["system-ssh-keys"], queryFn: client.systemSSHKeys });
  const [sshGithubUser, setSshGithubUser] = useState("");
  const [sshPaste, setSshPaste] = useState("");
  const [sshDeleteTarget, setSshDeleteTarget] = useState<SSHKey | null>(null);
  const sshAdd = useMutation({
    mutationFn: (input: { github_user?: string; keys?: string[] }) => client.addSystemSSHKeys(input),
    onSuccess: (data) => {
      queryClient.invalidateQueries({ queryKey: ["system-ssh-keys"] });
      toast.success(data.added === 0 ? "No new keys added (already present)" : `Added ${data.added} key(s)`);
      setSshGithubUser("");
      setSshPaste("");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Failed to add keys"),
  });
  const sshDelete = useMutation({
    mutationFn: (input: { fingerprint: string; confirm_last?: boolean }) => client.deleteSystemSSHKey(input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["system-ssh-keys"] });
      toast.success("Key revoked");
      setSshDeleteTarget(null);
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Failed to revoke key"),
  });

  function submitSSH(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const github = sshGithubUser.trim();
    const paste = sshPaste.trim();
    const keys = paste ? paste.split("\n").map((k) => k.trim()).filter(Boolean) : undefined;
    if (!github && (!keys || keys.length === 0)) {
      toast.error("Enter a GitHub username or paste at least one public key");
      return;
    }
    sshAdd.mutate({ github_user: github || undefined, keys });
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const formElement = event.currentTarget;
    const form = new FormData(formElement);
    const name = String(form.get("name") ?? "").trim();
    const email = String(form.get("email") ?? "").trim();
    create.mutate({ name, email, groups: createGroups }, { onSuccess: () => formElement.reset() });
  }

  function openGroupEditor(user: User) {
    setEditingUser(user);
    setEditGroups([...(user.groups ?? [])]);
  }

  return (
    <div className="page">
      <PageHeader eyebrow="Identity" title="People and recovery" description="Pocket ID enrollment and short-lived recovery. Omahab never verifies passwords or passkeys itself." />
      {recovery && <section className="recovery-banner" role="status"><div><strong>Recovery session created</strong><p>Expires {formatDate(recovery.expires_at)}. Share it only with the intended person over a trusted channel.</p>{recovery.login_url && <a href={recovery.login_url} target="_blank" rel="noreferrer">Open recovery sign-in</a>}{recovery.code && <><output className="recovery-code" aria-label="One-time recovery code">{recovery.code}</output> <CopyButton text={recovery.code} label="Copy code" /></>}</div><button className="icon-button" type="button" onClick={() => setRecovery(null)} aria-label="Dismiss">×</button></section>}
      {enrollmentUrl && (
        <section className="recovery-banner" role="status">
          <div>
            <strong>Enrollment link</strong>
            <p><a href={enrollmentUrl} target="_blank" rel="noreferrer">{enrollmentUrl}</a> <CopyButton text={enrollmentUrl} label="Copy" /></p>
            {enrollmentExpires && <small>Expires {enrollmentExpires}</small>}
            <p><small>Open this link in the user’s browser to register a passkey.</small></p>
            {welcomeUrlFromEnrollment(enrollmentUrl) ? (
              <p style={{ marginTop: 8 }}>
                <strong>Welcome page for household members:</strong>{" "}
                <a href={welcomeUrlFromEnrollment(enrollmentUrl)!} target="_blank" rel="noreferrer">{welcomeUrlFromEnrollment(enrollmentUrl)}</a>{" "}
                <CopyButton text={welcomeUrlFromEnrollment(enrollmentUrl)!} label="Copy welcome link" />
                <br />
                <small>Share this home.* link — it walks them through Tailscale, passkey setup, and opening Photos. Print or show the QR on the welcome page.</small>
              </p>
            ) : null}
          </div>
          <button className="icon-button" type="button" onClick={() => setEnrollmentUrl(null)} aria-label="Dismiss">×</button>
        </section>
      )}
      <div className="split-grid wide-primary">
        <Section title="Users" description="Disable access without deleting identity history.">
          {query.isLoading ? <LoadingState label="Loading users" /> : query.isError ? <ErrorState error={query.error} retry={() => void query.refetch()} /> : !users.length ? <EmptyState title="No users" description="Invite the first person to begin Pocket ID enrollment." /> : (
            <div className="compact-list">{users.map((user) => <div key={user.id}><div><strong>{user.name}</strong><span>{user.email} <CopyButton text={user.email} label="Copy" /></span><small>{(user.groups ?? []).join(", ") || "No groups"}</small></div><StatusPill value={user.disabled ? "disabled" : "active"} /><div className="row-actions"><button className="button ghost" type="button" onClick={() => openGroupEditor(user)}>Groups</button><button className="button ghost" type="button" disabled={recover.isPending || user.disabled} onClick={() => recover.mutate(user.id)}>Recover</button><button className="button ghost" type="button" disabled={enrollment.isPending} onClick={() => enrollment.mutate(user.id)}>Enrollment link</button><button className="button secondary" type="button" disabled={update.isPending} onClick={() => setToggleTarget(user)}>{user.disabled ? "Enable" : "Disable"}</button></div></div>)}</div>
          )}
          <OperationError error={update.error ?? recover.error ?? updateGroups.error ?? enrollment.error} />
        </Section>
        <Section title="Invite user" description="The person completes passkey enrollment through Pocket ID.">
          <form className="form-stack" onSubmit={submit}>
            <label>Name<input name="name" required autoComplete="name" /></label>
            <label>Email<input name="email" type="email" required autoComplete="email" /></label>
            <GroupMultiSelect values={createGroups} onChange={setCreateGroups} idPrefix="create-user" />
            <button className="button primary" type="submit" disabled={create.isPending}>{create.isPending ? "Inviting…" : "Invite user"}</button>
            <OperationError error={create.error} />
          </form>
        </Section>
      </div>
      <Section title="Server SSH access" description="These public keys grant Linux administrator SSH access. Private keys stay on the client and its password manager. You can reuse GitHub keys but Git-service authorization stays independent. Revoking a key blocks future SSH authentication, not already-open sessions. The WebUI and local password login remain usable after deleting the last key.">
        {sshQuery.isLoading ? <LoadingState label="Loading SSH keys" /> : sshQuery.isError ? <ErrorState error={sshQuery.error} retry={() => void sshQuery.refetch()} /> : (
          <>
            {sshQuery.data && (
              <p className="muted" style={{ marginBottom: 8 }}>Administrator username: <strong className="mono">{sshQuery.data.username}</strong> <CopyButton text={sshQuery.data.username} label="Copy username" /></p>
            )}
            {!sshQuery.data?.items.length ? (
              <EmptyState title="No SSH keys" description="Remote SSH stays unavailable until you add a key. Your local username and password still work." />
            ) : (
              <div className="compact-list">
                {sshQuery.data!.items.map((k) => (
                  <div key={k.fingerprint} style={{ display: "flex", flexDirection: "column", gap: 4, padding: "6px 0", borderBottom: "1px solid var(--line)" }}>
                    <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                      <span className="mono" style={{ fontSize: "0.9em" }}>{k.type}</span>
                      <span className="mono" style={{ fontSize: "0.85em", wordBreak: "break-all" }}>{k.fingerprint}</span>
                      <CopyButton text={k.fingerprint} label="Copy fingerprint" />
                      <span style={{ opacity: 0.7, fontSize: "0.85em" }}>{k.comment || "no comment"}</span>
                    </div>
                    <div className="row-actions">
                      <button className="button ghost" type="button" onClick={() => setSshDeleteTarget(k)}>Revoke</button>
                    </div>
                  </div>
                ))}
              </div>
            )}
            <form className="form-stack" onSubmit={submitSSH} style={{ marginTop: 16 }}>
              <label>GitHub username<input value={sshGithubUser} onChange={(e) => setSshGithubUser(e.target.value)} placeholder="your-github-username" autoComplete="off" /></label>
              <label>…or paste public keys (one per line, end with blank line)<textarea value={sshPaste} onChange={(e) => setSshPaste(e.target.value)} rows={4} placeholder="ssh-ed25519 AAAA…" className="mono" /></label>
              <button className="button primary" type="submit" disabled={sshAdd.isPending}>{sshAdd.isPending ? "Adding…" : "Add keys"}</button>
              <OperationError error={sshAdd.error} />
            </form>
          </>
        )}
      </Section>
      {editingUser && (
        <EditGroupsDialog
          user={editingUser}
          groups={editGroups}
          onChange={setEditGroups}
          onClose={() => setEditingUser(null)}
          onSave={() => updateGroups.mutate({ id: editingUser.id, groups: editGroups })}
          pending={updateGroups.isPending}
          error={updateGroups.error}
        />
      )}
      {toggleTarget && (
        <DestructiveConfirm
          title={toggleTarget.disabled ? `Enable ${toggleTarget.name}` : `Disable ${toggleTarget.name}`}
          description={toggleTarget.disabled ? `${toggleTarget.name} will regain access to enrolled applications.` : `${toggleTarget.name} will lose access immediately but identity history is retained.`}
          confirmValue={toggleTarget.email}
          confirmLabel="Type the email"
          onClose={() => setToggleTarget(null)}
          onConfirm={() => update.mutate({ id: toggleTarget.id, disabled: !toggleTarget.disabled })}
          pending={update.isPending}
          error={update.error}
        />
      )}
      {sshDeleteTarget && (
        <DestructiveConfirm
          title={`Revoke SSH key`}
          description={sshQuery.data && sshQuery.data.items.length === 1 ? "This is the last SSH key. Confirm removal to disable remote SSH access. Revoking blocks future SSH authentication, not already-open sessions. The WebUI and local password login remain usable." : "Revoking blocks future SSH authentication, not already-open sessions."}
          confirmValue={sshDeleteTarget.fingerprint}
          confirmLabel="Type the fingerprint to confirm"
          onClose={() => setSshDeleteTarget(null)}
          onConfirm={() => sshDelete.mutate({ fingerprint: sshDeleteTarget.fingerprint, confirm_last: sshQuery.data?.items.length === 1 })}
          pending={sshDelete.isPending}
          error={sshDelete.error}
        />
      )}
    </div>
  );
}

function EditGroupsDialog({
  user,
  groups,
  onChange,
  onClose,
  onSave,
  pending,
  error,
}: {
  user: User;
  groups: string[];
  onChange: (next: string[]) => void;
  onClose: () => void;
  onSave: () => void;
  pending?: boolean;
  error?: unknown;
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const dialog = dialogRef.current;
    const returnFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    if (dialog && !dialog.open) dialog.showModal();
    return () => {
      if (dialog?.open) dialog.close();
      returnFocus?.focus();
    };
  }, []);
  return (
    <dialog ref={dialogRef} className="modal" aria-labelledby="groups-title" onCancel={(event) => { event.preventDefault(); onClose(); }}>
      <header><div><p className="eyebrow">Groups</p><h2 id="groups-title">Groups for {user.name}</h2></div><button type="button" className="icon-button" onClick={onClose} aria-label="Close">×</button></header>
      <div className="form-stack">
        <p className="muted">{user.email}</p>
        <GroupMultiSelect values={groups} onChange={onChange} idPrefix={`edit-${user.id}`} />
        {error ? <p className="inline-error" role="alert">{error instanceof Error ? error.message : "Could not update groups."}</p> : null}
        <div className="modal-actions">
          <button type="button" className="button secondary" onClick={onClose}>Cancel</button>
          <button type="button" className="button primary" disabled={pending} onClick={onSave}>{pending ? "Saving…" : "Save groups"}</button>
        </div>
      </div>
    </dialog>
  );
}

