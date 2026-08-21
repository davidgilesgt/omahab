import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "../auth";
import type { RecoverySession } from "../api/types";
import { EmptyState, ErrorState, formatDate, LoadingState, PageHeader, Section, StatusPill } from "../components/ui";

function OperationError({ error }: { error: unknown }) {
  return error ? <p className="inline-error" role="alert">{error instanceof Error ? error.message : "The operation failed."}</p> : null;
}

export function SyncFoldersPage() {
  const { client } = useAuth();
  const queryClient = useQueryClient();
  const query = useQuery({ queryKey: ["sync-folders"], queryFn: client.syncFolders });
  const create = useMutation({
    mutationFn: client.createSyncFolder,
    onSuccess: async () => queryClient.invalidateQueries({ queryKey: ["sync-folders"] }),
  });
  const update = useMutation({
    mutationFn: ({ id, share }: { id: string; share: boolean }) => client.updateSyncFolder(id, { share_with_ai: share }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["sync-folders"] }),
  });
  const folders = query.data ?? [];

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const formElement = event.currentTarget;
    const form = new FormData(formElement);
    create.mutate({ name: String(form.get("name") ?? "").trim(), server_path: String(form.get("path") ?? "").trim(), share_with_ai: form.get("share") === "on" }, { onSuccess: () => formElement.reset() });
  }

  return (
    <div className="page">
      <PageHeader eyebrow="Knowledge" title="Sync folders" description="Manage server-side Syncthing folders and their explicit AI reading permission." />
      <div className="split-grid wide-primary">
        <Section title="Folders" description="Sharing with AI permits the default assistant to list, search, and read this folder.">
          {query.isLoading ? <LoadingState label="Loading folders" /> : query.isError ? <ErrorState error={query.error} retry={() => void query.refetch()} /> : !folders.length ? <EmptyState title="No synchronized folders" description="Add a server folder to begin syncing it with trusted devices." /> : (
            <div className="compact-list">{folders.map((folder) => <div key={folder.id}><div><strong>{folder.name}</strong><span className="mono">{folder.server_path}</span></div><StatusPill value={folder.health} /><label className="switch"><input type="checkbox" checked={folder.share_with_ai} disabled={update.isPending} onChange={(event) => update.mutate({ id: folder.id, share: event.currentTarget.checked })} /><span>Share with AI</span></label></div>)}</div>
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
  const workspaces = useQuery({ queryKey: ["workspaces"], queryFn: client.workspaces });
  const projects = useQuery({ queryKey: ["projects"], queryFn: client.projects });
  const create = useMutation({ mutationFn: client.createWorkspace, onSuccess: () => queryClient.invalidateQueries({ queryKey: ["workspaces"] }) });
  const stop = useMutation({ mutationFn: client.stopWorkspace, onSuccess: () => queryClient.invalidateQueries({ queryKey: ["workspaces"] }) });
  const workspaceItems = workspaces.data ?? [];
  const projectItems = projects.data ?? [];

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
              return <article className="resource-row" key={workspace.id}><div><div className="resource-title"><strong>{project?.name ?? workspace.project_id}</strong><StatusPill value={workspace.status} /></div><p><span className="mono">{workspace.branch}</span> · {workspace.agent}</p><small>Last active {formatDate(workspace.last_active_at)}{workspace.expires_at ? ` · expires ${formatDate(workspace.expires_at)}` : ""}</small></div>{workspace.status !== "stopped" && <button className="button secondary" type="button" disabled={stop.isPending} onClick={() => stop.mutate(workspace.id)}>Stop</button>}</article>})}</div>
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
    </div>
  );
}

export function PeoplePage() {
  const { client } = useAuth();
  const queryClient = useQueryClient();
  const query = useQuery({ queryKey: ["users"], queryFn: client.users });
  const [recovery, setRecovery] = useState<RecoverySession | null>(null);
  const create = useMutation({ mutationFn: client.createUser, onSuccess: () => queryClient.invalidateQueries({ queryKey: ["users"] }) });
  const update = useMutation({ mutationFn: ({ id, disabled }: { id: string; disabled: boolean }) => client.setUserDisabled(id, disabled), onSuccess: () => queryClient.invalidateQueries({ queryKey: ["users"] }) });
  const recover = useMutation({ mutationFn: client.beginRecovery, onSuccess: setRecovery });
  const users = query.data ?? [];

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const formElement = event.currentTarget;
    const form = new FormData(formElement);
    create.mutate({ name: String(form.get("name") ?? "").trim(), email: String(form.get("email") ?? "").trim() }, { onSuccess: () => formElement.reset() });
  }

  return (
    <div className="page">
      <PageHeader eyebrow="Identity" title="People and recovery" description="Pocket ID enrollment and short-lived recovery. Omahab never verifies passwords or passkeys itself." />
      {recovery && <section className="recovery-banner" role="status"><div><strong>Recovery session created</strong><p>Expires {formatDate(recovery.expires_at)}. Share it only with the intended person over a trusted channel.</p>{recovery.login_url && <a href={recovery.login_url} target="_blank" rel="noreferrer">Open recovery sign-in</a>}{recovery.code && <output className="recovery-code" aria-label="One-time recovery code">{recovery.code}</output>}</div><button className="icon-button" type="button" onClick={() => setRecovery(null)} aria-label="Dismiss">×</button></section>}
      <div className="split-grid wide-primary">
        <Section title="Users" description="Disable access without deleting identity history.">
          {query.isLoading ? <LoadingState label="Loading users" /> : query.isError ? <ErrorState error={query.error} retry={() => void query.refetch()} /> : !users.length ? <EmptyState title="No users" description="Invite the first person to begin Pocket ID enrollment." /> : (
            <div className="compact-list">{users.map((user) => <div key={user.id}><div><strong>{user.name}</strong><span>{user.email}</span><small>{user.groups.join(", ") || "No groups"}</small></div><StatusPill value={user.disabled ? "disabled" : "active"} /><div className="row-actions"><button className="button ghost" type="button" disabled={recover.isPending || user.disabled} onClick={() => recover.mutate(user.id)}>Recover</button><button className="button secondary" type="button" disabled={update.isPending} onClick={() => update.mutate({ id: user.id, disabled: !user.disabled })}>{user.disabled ? "Enable" : "Disable"}</button></div></div>)}</div>
          )}
          <OperationError error={update.error ?? recover.error} />
        </Section>
        <Section title="Invite user" description="The person completes passkey enrollment through Pocket ID.">
          <form className="form-stack" onSubmit={submit}><label>Name<input name="name" required autoComplete="name" /></label><label>Email<input name="email" type="email" required autoComplete="email" /></label><button className="button primary" type="submit" disabled={create.isPending}>{create.isPending ? "Inviting…" : "Invite user"}</button><OperationError error={create.error} /></form>
        </Section>
      </div>
    </div>
  );
}

export function ProvidersPage() {
  const { client } = useAuth();
  const queryClient = useQueryClient();
  const query = useQuery({ queryKey: ["provider-credentials"], queryFn: client.providerCredentials });
  const create = useMutation({
    mutationFn: client.createProviderCredential,
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["provider-credentials"] }),
  });
  const revoke = useMutation({ mutationFn: client.revokeProvider, onSuccess: () => queryClient.invalidateQueries({ queryKey: ["provider-credentials"] }) });
  const credentials = query.data ?? [];

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const formElement = event.currentTarget;
    const form = new FormData(formElement);
    const name = String(form.get("name") ?? "").trim();
    create.mutate({
      provider: String(form.get("provider") ?? "").trim(),
      kind: String(form.get("kind") ?? ""),
      value: String(form.get("value") ?? ""),
      ...(name ? { name } : {}),
    }, { onSuccess: () => formElement.reset() });
  }

  return (
    <div className="page">
      <PageHeader eyebrow="Model access" title="Provider credentials" description="Authorization state and entitlement metadata only. Stored tokens are never returned or displayed." />
      <div className="split-grid wide-primary">
        <Section title="Configured providers" description="Credentials are encrypted by the server’s secrets broker.">
          {query.isLoading ? <LoadingState label="Loading provider metadata" /> : query.isError ? <ErrorState error={query.error} retry={() => void query.refetch()} /> : !credentials.length ? <EmptyState title="No providers configured" description="Add a supported provider credential. Its value is sent once and never returned by the API." /> : (
            <div className="compact-list">{credentials.map((credential) => <div key={credential.id}><div><strong>{credential.name || credential.provider}</strong><span>{credential.kind} · updated {formatDate(credential.updated_at)}</span>{credential.expires_at && <small>Authorization expires {formatDate(credential.expires_at)}</small>}</div><StatusPill value={credential.status} /><button className="button secondary" type="button" disabled={revoke.isPending} onClick={() => revoke.mutate(credential.id)}>Revoke</button></div>)}</div>
          )}
          <OperationError error={revoke.error} />
        </Section>
        <Section title="Add credential" description="The value is write-only. After submission, only safe metadata remains visible.">
          <form className="form-stack" onSubmit={submit}>
            <label>Provider<input name="provider" required autoComplete="off" /></label>
            <label>Display name <span className="muted">(optional)</span><input name="name" autoComplete="off" /></label>
            <label>Credential kind<select name="kind" defaultValue="api_key"><option value="api_key">API key</option><option value="oauth">OAuth credential</option></select></label>
            <label>Credential value<input name="value" type="password" required autoComplete="new-password" /></label>
            <button className="button primary" type="submit" disabled={create.isPending}>{create.isPending ? "Saving… " : "Save credential"}</button>
            <OperationError error={create.error} />
          </form>
        </Section>
      </div>
    </div>
  );
}
