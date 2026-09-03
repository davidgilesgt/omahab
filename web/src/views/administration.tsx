// P1-3 integrator note: No dedicated knowledge/assistant settings view exists in web/src/views.
// The required UI (three semantic-index options: Best English / Best worldwide / Full-text only,
// pinned-model metadata from pinned_models.json, and summarization-consent dialog with provider
// and informed choice) should be implemented as a new route (e.g., /knowledge or /ai/settings)
// or integrated into the Sync folders view once API routes /api/v1/knowledge/* are available.
// Workers/embedding/pinned_models.json.example contains the model metadata shape.
import { useEffect, useRef, useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "../auth";
import type { CreateModelKeyResponse, ModelAlias, ModelAliasName, ModelKey, OAuthSession, ProviderCredential, RecoverySession, User, Workspace } from "../api/types";
import { EmptyState, ErrorState, formatDate, LoadingState, PageHeader, Section, StatusPill } from "../components/ui";
import { useToast } from "../components/toast";
import { CopyButton } from "../components/copyButton";

const GROUP_OPTIONS = ["admins", "members", "guests"] as const;

function OperationError({ error }: { error: unknown }) {
  return error ? <p className="inline-error" role="alert">{error instanceof Error ? error.message : "The operation failed."}</p> : null;
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
      {enrollmentUrl && <section className="recovery-banner" role="status"><div><strong>Enrollment link</strong><p><a href={enrollmentUrl} target="_blank" rel="noreferrer">{enrollmentUrl}</a> <CopyButton text={enrollmentUrl} label="Copy" /></p>{enrollmentExpires && <small>Expires {enrollmentExpires}</small>}<p><small>Open this link in the user’s browser to register a passkey.</small></p></div><button className="icon-button" type="button" onClick={() => setEnrollmentUrl(null)} aria-label="Dismiss">×</button></section>}
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

const SUPPORTED_PROVIDERS = [
  { value: "openai", label: "OpenAI", kinds: ["api_key"] as const },
  { value: "anthropic", label: "Anthropic", kinds: ["api_key"] as const },
  { value: "openrouter", label: "OpenRouter", kinds: ["api_key"] as const },
  { value: "chatgpt", label: "ChatGPT (subscription)", kinds: ["oauth"] as const },
  { value: "xai", label: "xAI Grok (subscription)", kinds: ["oauth"] as const },
] as const;

const ALIAS_NAMES: ModelAliasName[] = ["omahab/fast", "omahab/balanced", "omahab/reasoning", "omahab/embedding"];

function EntitlementPill({ value }: { value?: string | null }) {
  const v = (value ?? "unknown").toLowerCase();
  return <StatusPill value={v} />;
}

export function ProvidersPage() {
  const { client } = useAuth();
  const queryClient = useQueryClient();
  const toast = useToast();
  const query = useQuery({ queryKey: ["provider-credentials"], queryFn: client.providerCredentials });
  const aliasesQuery = useQuery({ queryKey: ["model-aliases"], queryFn: client.modelAliases });
  const keysQuery = useQuery({ queryKey: ["model-keys"], queryFn: client.modelKeys });
  const create = useMutation({
    mutationFn: client.createProviderCredential,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["provider-credentials"] });
      toast.success("Credential saved");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Could not save credential"),
  });
  const revoke = useMutation({
    mutationFn: client.revokeProvider,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["provider-credentials"] });
      toast.success("Provider revoked");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Revoke failed"),
  });
  const setAlias = useMutation({
    mutationFn: ({ name, credential_id, model, fallback_order }: { name: ModelAliasName; credential_id: string; model: string; fallback_order?: string[] }) =>
      client.setModelAlias(name, { credential_id, model, fallback_order }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["model-aliases"] });
      toast.success("Alias updated");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Could not update alias"),
  });
  const createKey = useMutation({
    mutationFn: client.createModelKey,
    onSuccess: (data: CreateModelKeyResponse) => {
      queryClient.invalidateQueries({ queryKey: ["model-keys"] });
      const keyValue = data.key ?? "";
      if (keyValue) {
        toast.success(`Key created: ${keyValue.slice(0, 12)}… (copied once)`);
      } else {
        toast.success("Key created");
      }
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Could not create key"),
  });
  const revokeKey = useMutation({
    mutationFn: client.deleteModelKey,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["model-keys"] });
      toast.success("Key revoked");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Could not revoke key"),
  });

  const credentials = query.data ?? [];
  const aliases = aliasesQuery.data ?? [];
  const keys = keysQuery.data ?? [];
  const [revokeTarget, setRevokeTarget] = useState<ProviderCredential | null>(null);
  const [selectedProvider, setSelectedProvider] = useState<string>("openai");
  const [selectedKind, setSelectedKind] = useState<string>("api_key");
  const [aliasDrafts, setAliasDrafts] = useState<Record<string, { credential_id: string; model: string; fallback: string }>>({});
  const [keyForm, setKeyForm] = useState({ name: "", owner_kind: "hermes" as const, owner_id: "default", scopes: [] as ModelAliasName[], rpm: "", tpm: "", concurrency: "", budget: "" });
  const [createdKeyPlaintext, setCreatedKeyPlaintext] = useState<string | null>(null);
  const [oauthSessions, setOAuthSessions] = useState<Record<string, OAuthSession | null>>({});
  const [oauthError, setOAuthError] = useState<Record<string, string | null>>({});
  const [polling, setPolling] = useState<Record<string, boolean>>({});
  const [toolsetPending, setToolsetPending] = useState<Record<string, boolean>>({});
useEffect(() => {
    const allowed = allowedKindsForProvider(selectedProvider);
    if (!allowed.includes(selectedKind as never)) {
      setSelectedKind(allowed[0] ?? "api_key");
    }
  }, [selectedProvider, selectedKind]);

  // Keep alias drafts in sync with fetched aliases
  useEffect(() => {
    if (!aliasesQuery.data) return;
    const next: Record<string, { credential_id: string; model: string; fallback: string }> = {};
    for (const name of ALIAS_NAMES) {
      const alias = aliasesQuery.data.find((a) => a.name === name);
      next[name] = {
        credential_id: alias?.credential_id ?? "",
        model: alias?.model ?? "",
        fallback: alias?.fallback_order?.join(", ") ?? "",
      };
    }
    setAliasDrafts(next);
  }, [aliasesQuery.data]);

  // Polling effect for active OAuth sessions
  useEffect(() => {
    const providers = Object.keys(polling).filter((p) => polling[p]);
    if (!providers.length) return;
    const interval = window.setInterval(async () => {
      for (const provider of providers) {
        const sess = oauthSessions[provider];
        if (!sess?.id) continue;
        try {
          const updated = await client.pollProviderOAuth(provider as "chatgpt" | "xai", sess.id);
          setOAuthSessions((prev) => ({ ...prev, [provider]: updated }));
          if (["connected", "denied", "expired", "error"].includes(updated.status)) {
            setPolling((prev) => ({ ...prev, [provider]: false }));
            if (updated.status === "connected") {
              toast.success(`${provider} connected`);
              void queryClient.invalidateQueries({ queryKey: ["provider-credentials"] });
            } else if (updated.status === "denied") toast.error(`${provider} authorization denied`);
            else if (updated.status === "expired") toast.error(`${provider} authorization expired`);
            else if (updated.status === "error") toast.error(`${provider} authorization error`);
          }
        } catch (err) {
          const msg = err instanceof Error ? err.message : "Poll failed";
          setOAuthError((prev) => ({ ...prev, [provider]: msg }));
          if (msg.includes("403") || msg.toLowerCase().includes("tier") || msg.toLowerCase().includes("not_entitled")) {
            setPolling((prev) => ({ ...prev, [provider]: false }));
          }
        }
      }
    }, 5000);
    return () => window.clearInterval(interval);
  }, [polling, oauthSessions, client, queryClient, toast]);
function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const formElement = event.currentTarget;
    const form = new FormData(formElement);
    const provider = String(form.get("provider") ?? "").trim();
    const kind = String(form.get("kind") ?? "").trim();
    const name = String(form.get("name") ?? "").trim();
    const value = String(form.get("value") ?? "");
    const allowed = allowedKindsForProvider(provider);
    if (!SUPPORTED_PROVIDERS.some((p) => p.value === provider)) {
      toast.error("Unsupported provider");
      return;
    }
    if (!allowed.includes(kind as never)) {
      toast.error(`Provider ${provider} only supports ${allowed.join(", ")}`);
      return;
    }
    if (kind === "api_key" && !value.trim()) {
      toast.error("Credential value required for API key");
      return;
    }
    if (kind === "oauth" && value.trim()) {
      toast.error("OAuth credentials do not use a value — use Start OAuth instead");
      return;
    }
    create.mutate(
      {
        provider,
        kind,
        value: kind === "oauth" ? "" : value,
        ...(name ? { name } : {}),
      },
      {
        onSuccess: () => formElement.reset(),
      },
    );
  }

  async function startOAuth(provider: "chatgpt" | "xai") {
    const flow = provider === "chatgpt" ? "device_code" : "loopback";
    setOAuthError((prev) => ({ ...prev, [provider]: null }));
    try {
      const sess = await client.startProviderOAuth(provider, flow as "device_code" | "loopback");
      setOAuthSessions((prev) => ({ ...prev, [provider]: sess }));
      setPolling((prev) => ({ ...prev, [provider]: true }));
      toast.success(`${provider} OAuth started`);
    } catch (err) {
      const msg = err instanceof Error ? err.message : "Could not start OAuth";
      setOAuthError((prev) => ({ ...prev, [provider]: msg }));
      toast.error(msg);
    }
  }

  function handleAliasSave(name: ModelAliasName) {
    const draft = aliasDrafts[name];
    if (!draft) return;
    if (!draft.credential_id.trim()) {
      toast.error("Select a credential for alias");
      return;
    }
    if (!draft.model.trim()) {
      toast.error("Model must be non-empty");
      return;
    }
    const fallback = draft.fallback.trim() ? draft.fallback.split(",").map((s) => s.trim()).filter(Boolean) : undefined;
    setAlias.mutate({ name, credential_id: draft.credential_id.trim(), model: draft.model.trim(), fallback_order: fallback });
  }

  function handleCreateKey(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const rpm = keyForm.rpm ? Number(keyForm.rpm) : undefined;
    const tpm = keyForm.tpm ? Number(keyForm.tpm) : undefined;
    const concurrency = keyForm.concurrency ? Number(keyForm.concurrency) : undefined;
    const budget = keyForm.budget ? Number(keyForm.budget) : undefined;
    if (!keyForm.name.trim()) {
      toast.error("Key name required");
      return;
    }
    if (!keyForm.owner_id.trim()) {
      toast.error("Owner ID required");
      return;
    }
    createKey.mutate(
      {
        name: keyForm.name.trim(),
        owner_kind: keyForm.owner_kind,
        owner_id: keyForm.owner_id.trim(),
        scopes: keyForm.scopes.length ? keyForm.scopes : undefined,
        rpm: rpm,
        tpm: tpm,
        concurrency: concurrency,
        budget: budget,
      },
      {
        onSuccess: (data: CreateModelKeyResponse) => {
          if (data.key) setCreatedKeyPlaintext(data.key);
        },
      },
    );
  }

  return (
    <div className="page">
      <PageHeader eyebrow="Model access" title="Provider credentials" description="Authorization state and entitlement metadata only. Stored tokens are never returned or displayed." />
      <div className="split-grid wide-primary">
        <Section title="Configured providers" description="Credentials are encrypted by the server’s secrets broker. Managed by omahab (API key) or litellm (subscription).">
          {query.isLoading ? (
            <LoadingState label="Loading provider metadata" />
          ) : query.isError ? (
            <ErrorState error={query.error} retry={() => void query.refetch()} />
          ) : !credentials.length ? (
            <EmptyState title="No providers configured" description="Add a supported provider credential. Its value is sent once and never returned by the API." />
          ) : (
            <div className="compact-list">
              {credentials.map((credential) => (
                <div key={credential.id} style={{ display: "flex", gap: "0.75rem", justifyContent: "space-between", alignItems: "flex-start", padding: "0.5rem 0", borderBottom: "1px solid var(--border)" }}>
                  <div style={{ flex: 1 }}>
                    <strong>{credential.name || credential.provider}</strong>
                    <div className="muted" style={{ fontSize: "0.875rem" }}>
                      {credential.provider} · {credential.kind} · managed by {credential.managed_by}
                      {credential.external_ref ? ` · ${credential.external_ref}` : ""}
                    </div>
                    <div className="muted" style={{ fontSize: "0.8rem" }}>updated {formatDate(credential.updated_at)}</div>
                    {credential.expires_at && <small>Authorization expires {formatDate(credential.expires_at)}</small>}
                    <div style={{ display: "flex", gap: "0.5rem", marginTop: "0.25rem", flexWrap: "wrap", alignItems: "center" }}>
                      <StatusPill value={credential.status} />
                      <EntitlementPill value={credential.entitlement} />
                      {credential.entitlement === "not_entitled" && credential.provider === "xai" && (
                        <span className="inline-error" style={{ fontSize: "0.8rem" }}>Tier restriction: use API-key path <a href="#add-credential">create API key</a></span>
                      )}
                    </div>
                  </div>
                  <div style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
                    <button className="button secondary" type="button" disabled={revoke.isPending} onClick={() => setRevokeTarget(credential)}>
                      Revoke
                    </button>
                    {(credential.provider === "chatgpt" || credential.provider === "xai") && credential.kind === "oauth" && (
                      <button className="button ghost" type="button" onClick={() => startOAuth(credential.provider as "chatgpt" | "xai")} disabled={!!polling[credential.provider]}>
                        Reauthorize
                      </button>
                    )}
                  </div>
                </div>
              ))}
            </div>
          )}
          <OperationError error={revoke.error} />
        </Section>
        <Section title="Add credential" description="The value is write-only. After submission, only safe metadata remains visible." >
          <form className="form-stack" onSubmit={submit} id="add-credential">
            <label>
              Provider
              <select name="provider" required value={selectedProvider} onChange={(e) => setSelectedProvider(e.target.value)}>
                {SUPPORTED_PROVIDERS.map((p) => (
                  <option key={p.value} value={p.value}>
                    {p.label}
                  </option>
                ))}
              </select>
              <small className="muted">OpenAI/Anthropic/OpenRouter use API key; ChatGPT/xAI use OAuth (managed by LiteLLM).</small>
            </label>
            <label>
              Display name <span className="muted">(optional)</span>
              <input name="name" autoComplete="off" />
            </label>
            <label>
              Credential kind
              <select name="kind" value={selectedKind} onChange={(e) => setSelectedKind(e.target.value)} required>
                <option value="api_key" disabled={!allowedKindsForProvider(selectedProvider).includes("api_key")}>
                  API key {allowedKindsForProvider(selectedProvider).includes("api_key") ? "" : "(not supported for this provider)"}
                </option>
                <option value="oauth" disabled={!allowedKindsForProvider(selectedProvider).includes("oauth")}>
                  OAuth credential {allowedKindsForProvider(selectedProvider).includes("oauth") ? "" : "(not supported for this provider)"}
                </option>
              </select>
            </label>
            {selectedKind === "api_key" ? (
              <label>
                Credential value
                <input name="value" type="password" required autoComplete="new-password" />
                <small className="muted">Write-once; only prefix remains visible.</small>
              </label>
            ) : (
              <p className="muted" style={{ fontSize: "0.875rem" }}>OAuth credentials are created via Start OAuth below — no value needed here. Submitting here creates a placeholder managed by LiteLLM.</p>
            )}
            <button className="button primary" type="submit" disabled={create.isPending}>
              {create.isPending ? "Saving… " : "Save credential"}
            </button>
            <OperationError error={create.error} />
          </form>
        </Section>
      </div>

      <Section title="Subscription OAuth" description="ChatGPT uses device_code; xAI uses loopback at 127.0.0.1:56121 via companion relay. Never expose device codes or tokens; only verification URLs and user codes are shown.">
        <div className="split-grid">
          {(["chatgpt", "xai"] as const).map((provider) => {
            const sess = oauthSessions[provider];
            const isPolling = !!polling[provider];
            const err = oauthError[provider];
            const isXai = provider === "xai";
            return (
              <div key={provider} className="form-stack" style={{ border: "1px solid var(--border)", padding: "1rem", borderRadius: "8px" }}>
                <h3 style={{ margin: 0 }}>{provider === "chatgpt" ? "ChatGPT" : "xAI Grok"} — {isXai ? "loopback" : "device_code"}</h3>
                <button className="button secondary" type="button" disabled={isPolling} onClick={() => startOAuth(provider)}>
                  {isPolling ? "Authorizing…" : `Start ${provider} OAuth`}
                </button>
                {sess && (
                  <div className="form-stack" style={{ gap: "0.5rem", fontSize: "0.9rem" }}>
                    <div>
                      <strong>Status:</strong> <StatusPill value={sess.status} />
                    </div>
                    <div>
                      <strong>Verification URL:</strong> <a href={sess.verification_url} target="_blank" rel="noreferrer">{sess.verification_url}</a> <CopyButton text={sess.verification_url} label="Copy" />
                    </div>
                    {sess.user_code && (
                      <div>
                        <strong>User code:</strong> <code className="mono">{sess.user_code}</code> <CopyButton text={sess.user_code} label="Copy" />
                      </div>
                    )}
                    {sess.callback_port && <div><strong>Callback port:</strong> {sess.callback_port} (fixed 56121)</div>}
                    <div><small className="muted">Expires {formatDate(sess.expires_at)}</small></div>
                    {isPolling && <small className="muted">Polling every 5s until connected/denied/expired/error…</small>}
                  </div>
                )}
                {err && (
                  <p className="inline-error" role="alert">
                    {err}{" "}
                    {(err.includes("403") || err.toLowerCase().includes("tier") || err.toLowerCase().includes("not_entitled")) && isXai && (
                      <span>Tier restriction: use API-key path. <a href="#add-credential">Create API key</a></span>
                    )}
                  </p>
                )}
                {sess?.status === "error" && isXai && (
                  <p className="inline-error">Tier restriction: use API-key path. Current subscription does not entitle Grok via OAuth. <a href="#add-credential">Create API key</a></p>
                )}
              </div>
            );
          })}
        </div>
      </Section>

      <Section title="Model aliases" description="Four stable aliases route via LiteLLM. Default no fallback; add explicit ordered fallbacks only when needed.">
        {aliasesQuery.isLoading ? (
          <LoadingState label="Loading aliases" />
        ) : aliasesQuery.isError ? (
          <ErrorState error={aliasesQuery.error} retry={() => void aliasesQuery.refetch()} />
        ) : (
          <div className="form-stack">
            <table className="compact-table" style={{ width: "100%" }}>
              <thead>
                <tr>
                  <th>Alias</th>
                  <th>Credential</th>
                  <th>Model</th>
                  <th>Fallback order</th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                {ALIAS_NAMES.map((name) => {
                  const draft = aliasDrafts[name] ?? { credential_id: "", model: "", fallback: "" };
                  const current = aliases.find((a) => a.name === name);
                  return (
                    <tr key={name}>
                      <td><code>{name}</code></td>
                      <td>
                        <select value={draft.credential_id} onChange={(e) => setAliasDrafts((prev) => ({ ...prev, [name]: { ...draft, credential_id: e.target.value } }))}>
                          <option value="">Select credential</option>
                          {credentials.map((c) => (
                            <option key={c.id} value={c.id}>
                              {c.provider} · {c.name || c.id.slice(0, 8)} ({c.managed_by})
                            </option>
                          ))}
                        </select>
                        {current?.fallback_order?.length ? <small className="muted">primary → fallbacks: {current.fallback_order.join(", ")}</small> : <small className="muted">primary only (no fallback)</small>}
                      </td>
                      <td>
                        <input value={draft.model} placeholder="e.g. gpt-4o or xai/grok-4" onChange={(e) => setAliasDrafts((prev) => ({ ...prev, [name]: { ...draft, model: e.target.value } }))} />
                      </td>
                      <td>
                        <input value={draft.fallback} placeholder="fallback credential IDs, comma separated" onChange={(e) => setAliasDrafts((prev) => ({ ...prev, [name]: { ...draft, fallback: e.target.value } }))} />
                      </td>
                      <td>
                        <button className="button secondary" type="button" disabled={setAlias.isPending} onClick={() => handleAliasSave(name)}>
                          Save
                        </button>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
            <OperationError error={setAlias.error ?? aliasesQuery.error} />
          </div>
        )}
      </Section>

      <Section title="Virtual keys" description="Distinct LiteLLM keys per hermes/device/harness. Plaintext returned only once on creation; listing shows prefix/owner/scopes/limits.">
        <div className="split-grid wide-primary">
          <div className="form-stack">
            {keysQuery.isLoading ? (
              <LoadingState label="Loading keys" />
            ) : keysQuery.isError ? (
              <ErrorState error={keysQuery.error} retry={() => void keysQuery.refetch()} />
            ) : !keys.length ? (
              <EmptyState title="No virtual keys" description="Issue a scoped key for a hermes instance, companion device, or harness." />
            ) : (
              <div className="compact-list">
                {keys.map((k: ModelKey) => (
                  <div key={k.id} style={{ display: "flex", justifyContent: "space-between", alignItems: "flex-start", gap: "1rem", padding: "0.5rem 0", borderBottom: "1px solid var(--border)" }}>
                    <div>
                      <strong>{k.name}</strong> <code className="mono">{k.key_prefix}…</code>
                      <div className="muted" style={{ fontSize: "0.8rem" }}>{k.owner_kind}/{k.owner_id} · created {formatDate(k.created_at)}</div>
                      <div style={{ display: "flex", gap: "0.25rem", flexWrap: "wrap", marginTop: "0.25rem" }}>
                        {(k.scopes ?? []).map((s) => (
                          <span key={s} className="chip" style={{ border: "1px solid var(--border)", padding: "0.1rem 0.4rem", borderRadius: "4px", fontSize: "0.75rem" }}>
                            {s}
                          </span>
                        ))}
                        {k.rpm ? <span className="muted" style={{ fontSize: "0.75rem" }}>RPM {k.rpm}</span> : null}
                        {k.tpm ? <span className="muted" style={{ fontSize: "0.75rem" }}>TPM {k.tpm}</span> : null}
                        {k.concurrency ? <span className="muted" style={{ fontSize: "0.75rem" }}>conc {k.concurrency}</span> : null}
                        {k.budget ? <span className="muted" style={{ fontSize: "0.75rem" }}>budget {k.budget}</span> : null}
                      </div>
                    </div>
                    <button className="button secondary" type="button" disabled={revokeKey.isPending} onClick={() => revokeKey.mutate(k.id)}>
                      Revoke
                    </button>
                  </div>
                ))}
              </div>
            )}
            <OperationError error={revokeKey.error} />
          </div>
          <form className="form-stack" onSubmit={handleCreateKey}>
            <h3 style={{ margin: 0 }}>Create virtual key</h3>
            <label>
              Name
              <input value={keyForm.name} onChange={(e) => setKeyForm((p) => ({ ...p, name: e.target.value }))} required />
            </label>
            <label>
              Owner kind
              <select value={keyForm.owner_kind} onChange={(e) => setKeyForm((p) => ({ ...p, owner_kind: e.target.value as never }))}>
                <option value="hermes">hermes</option>
                <option value="device">device</option>
                <option value="harness">harness</option>
              </select>
            </label>
            <label>
              Owner ID
              <input value={keyForm.owner_id} onChange={(e) => setKeyForm((p) => ({ ...p, owner_id: e.target.value }))} required />
            </label>
            <fieldset style={{ border: 0, padding: 0, margin: 0 }}>
              <legend className="muted" style={{ fontSize: "0.875rem" }}>Scopes</legend>
              <div style={{ display: "flex", gap: "0.5rem", flexWrap: "wrap" }}>
                {ALIAS_NAMES.map((alias) => (
                  <label key={alias} className="check-row" style={{ gap: "0.25rem" }}>
                    <input type="checkbox" checked={keyForm.scopes.includes(alias)} onChange={(e) => setKeyForm((p) => ({ ...p, scopes: e.target.checked ? [...p.scopes, alias] : p.scopes.filter((s) => s !== alias) }))} />
                    <span>{alias}</span>
                  </label>
                ))}
              </div>
            </fieldset>
            <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: "0.5rem" }}>
              <label>RPM <input value={keyForm.rpm} onChange={(e) => setKeyForm((p) => ({ ...p, rpm: e.target.value }))} type="number" min={1} /></label>
              <label>TPM <input value={keyForm.tpm} onChange={(e) => setKeyForm((p) => ({ ...p, tpm: e.target.value }))} type="number" min={1} /></label>
              <label>Concurrency <input value={keyForm.concurrency} onChange={(e) => setKeyForm((p) => ({ ...p, concurrency: e.target.value }))} type="number" min={1} /></label>
              <label>Budget <input value={keyForm.budget} onChange={(e) => setKeyForm((p) => ({ ...p, budget: e.target.value }))} type="number" min={0} step="0.01" /></label>
            </div>
            <button className="button primary" type="submit" disabled={createKey.isPending}>
              {createKey.isPending ? "Issuing…" : "Issue key"}
            </button>
            <OperationError error={createKey.error} />
            {createdKeyPlaintext && (
              <div className="recovery-banner" role="status">
                <div>
                  <strong>Key issued — copy now (shown once)</strong>
                  <p>
                    <code className="mono">{createdKeyPlaintext}</code> <CopyButton text={createdKeyPlaintext} label="Copy" />
                  </p>
                  <small className="muted">Plaintext is never stored; only prefix remains afterwards.</small>
                </div>
                <button className="icon-button" type="button" onClick={() => setCreatedKeyPlaintext(null)} aria-label="Dismiss">×</button>
              </div>
            )}
          </form>
        </div>
      </Section>

      {revokeTarget && (
        <DestructiveConfirm
          title={`Revoke ${revokeTarget.name || revokeTarget.provider}`}
          description={`Revoke ${revokeTarget.provider} (${revokeTarget.kind})? The stored credential will be deleted and model access will be removed.`}
          confirmValue={revokeTarget.provider}
          confirmLabel="Type the provider name"
          onClose={() => setRevokeTarget(null)}
          onConfirm={() => revoke.mutate(revokeTarget.id)}
          pending={revoke.isPending}
          error={revoke.error}
        />
      )}
    </div>
  );
}
