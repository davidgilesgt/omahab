import { Fragment, useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { Link, useLocation } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "../auth";
import type { Application, Backup, Exposure, Project, Release } from "../api/types";
import { EmptyState, ErrorState, formatDate, humanizeStatus, LoadingState, PageHeader, Section, shortDigest, StatusPill } from "../components/ui";
import { QRCode } from "../components/qr";
import { useToast } from "../components/toast";
import { CopyButton } from "../components/copyButton";
import { AppIcon } from "../components/appIcon";

function MutationNotice({ error }: { error: unknown }) {
  if (!error) return null;
  return <p className="inline-error" role="alert">{error instanceof Error ? error.message : "The operation failed."}</p>;
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
      <header><div><h2 id="confirm-title">{title}</h2></div><button type="button" className="icon-button" onClick={onClose} aria-label="Close">×</button></header>
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

export function OverviewPage() {
  const { client } = useAuth();
  const { pathname } = useLocation();
  const adminPrefix = pathname.startsWith("/admin") ? "/admin" : "";
  const status = useQuery({ queryKey: ["status"], queryFn: client.status });
  const applications = useQuery({ queryKey: ["applications"], queryFn: client.applications });
  const backups = useQuery({ queryKey: ["backups"], queryFn: client.backups });
  const events = useQuery({ queryKey: ["events"], queryFn: client.events });
  const setup = useQuery({ queryKey: ["setup"], queryFn: client.setup, retry: false, staleTime: 30_000 });

  if (status.isLoading) return <LoadingState label="Checking your server" />;
  if (status.isError) return <ErrorState error={status.error} retry={() => void status.refetch()} />;
  if (!status.data) return <LoadingState label="Checking your server" />;

  const unhealthy = applications.data?.filter((application) => application.health === "unhealthy" || application.health === "degraded") ?? [];
  const latestBackup = backups.data?.reduce<Backup | undefined>(
    (latest, backup) => !latest || Date.parse(backup.started_at) > Date.parse(latest.started_at) ? backup : latest,
    undefined,
  );
  const unread = events.data?.filter((event) => !event.read_at) ?? [];
  const launchers = (applications.data ?? []).filter((application) => application.launch_url);

  return (
    <div className="page">
      <PageHeader title="Overview" />
      <section className="launcher-strip" aria-label="Installed apps">
        {applications.isLoading ? <LoadingState label="Loading app launchers" /> : applications.isError ? (
          <ErrorState error={applications.error} retry={() => void applications.refetch()} />
        ) : launchers.length === 0 ? (
          <EmptyState title="No app launchers available" description="Installed apps with a browser interface appear here." action={<Link className="button secondary" to={`${adminPrefix}/applications`}>Applications</Link>} />
        ) : (
          <ul>
            {launchers.map((application) => {
              const url = application.launch_url ?? "";
              return (
                <li key={application.id}>
                  <a href={url} target="_blank" rel="noreferrer" title={url}>
                    <AppIcon bundleId={application.bundle_id} />
                    <span>{application.name}</span>
                    <small>{humanizeStatus(application.health)}{application.observed_state === "running" ? "" : ` · ${humanizeStatus(application.observed_state)}`}</small>
                  </a>
                </li>
              );
            })}
          </ul>
        )}
      </section>
      {setup.data && !setup.data.local_ready && (
        <p className="setup-notice" role="status"><strong>Setup is not finished</strong> — <Link to={`${adminPrefix}/setup`}>Continue setup</Link></p>
      )}
      {setup.data && setup.data.local_ready && setup.data.state !== "complete" && (
        <p className="setup-notice" role="status"><strong>Local control panel ready</strong> — <Link to={`${adminPrefix}/setup`}>Connect your services</Link></p>
      )}
      <div className="metric-strip">
        <article><span>Control plane</span><strong><StatusPill value={status.data.health} /></strong><small>Version {status.data.version}</small></article>
        <article><span>Applications</span><strong>{applications.data?.length ?? "—"}</strong><small>{unhealthy.length ? `${unhealthy.length} need attention` : "No reported issues"}</small></article>
        <article><span>Unread events</span><strong>{events.data ? unread.length : "—"}</strong><small>Operational inbox</small></article>
        <article><span>Verified recovery</span><strong>{latestBackup?.verified_at ? "Current" : "Not verified"}</strong><small>{latestBackup?.verified_at ? formatDate(latestBackup.verified_at) : "Run a restore verification"}</small></article>
      </div>
      <div className="split-grid">
        <Section title="Needs attention">
          {applications.isLoading || events.isLoading ? <LoadingState label="Checking services and events" /> : applications.isError || events.isError ? (
            <ErrorState error={applications.error ?? events.error} />
          ) : unhealthy.length === 0 && unread.length === 0 ? (
            <EmptyState title="Everything is quiet" description="No services or control-plane events currently need your attention." />
          ) : (
            <ul className="activity-list">
              {unhealthy.map((application) => <li key={application.id}><StatusPill value={application.health} /><div><strong>{application.name}</strong><span>Observed state: {application.observed_state}</span></div></li>)}
              {unread.slice(0, 5).map((event) => <li key={event.id}><StatusPill value={event.severity} /><div><strong>{event.message}</strong><span>{formatDate(event.created_at)}</span></div></li>)}
            </ul>
          )}
        </Section>
        <Section title="Recovery posture">
          {backups.isLoading ? <LoadingState label="Loading backups" /> : backups.isError ? <ErrorState error={backups.error} /> : latestBackup ? (
            <dl className="definition-list">
              <div><dt>Last backup</dt><dd>{formatDate(latestBackup.finished_at ?? latestBackup.started_at)}</dd></div>
              <div><dt>Snapshot</dt><dd className="mono">{latestBackup.snapshot_id ? <><span>{shortDigest(latestBackup.snapshot_id)}</span> <CopyButton text={latestBackup.snapshot_id} label="Copy" /></> : "Pending"}</dd></div>
              <div><dt>Restore verified</dt><dd>{formatDate(latestBackup.verified_at)}</dd></div>
              <div><dt>Status</dt><dd><StatusPill value={latestBackup.status} /></dd></div>
            </dl>
          ) : <EmptyState title="No backups yet" description="Create the first encrypted backup from the Backups page, then verify it can be restored." />}
        </Section>
      </div>
    </div>
  );
}

interface ExposureReviewProps {
  resource: "applications" | "projects";
  item: Application | Project;
  onClose: () => void;
}

function ExposureReview({ resource, item, onClose }: ExposureReviewProps) {
  const { client } = useAuth();
  const queryClient = useQueryClient();
  const toast = useToast();
  const [mode, setMode] = useState<Exposure>(item.exposure);
  const [confirmation, setConfirmation] = useState("");
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
  const mutation = useMutation({
    mutationFn: () => client.setExposure(resource, item.id, mode, confirmation || undefined),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: [resource] });
      toast.success("Exposure updated");
      onClose();
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Could not update exposure"),
  });
  const publicConfirmed = mode !== "public" || confirmation === item.hostname;

  return (
    <dialog ref={dialogRef} className="modal" aria-labelledby="exposure-title" onCancel={(event) => { event.preventDefault(); onClose(); }}>
        <header><div><h2 id="exposure-title">Exposure for {item.name}</h2></div><button type="button" className="icon-button" onClick={onClose} aria-label="Close">×</button></header>
        <div className="form-stack">
          <label>Exposure mode
            <select value={mode} onChange={(event) => { setMode(event.currentTarget.value as Exposure); setConfirmation(""); }} autoFocus>
              <option value="private">Private · tailnet only</option>
              <option value="shared">Shared · invited users</option>
              <option value="public">Public · internet reachable</option>
            </select>
          </label>
          <dl className="review-list">
            <div><dt>Resulting hostname</dt><dd className="mono">{item.hostname} <CopyButton text={item.hostname} label="Copy" /></dd></div>
            <div><dt>Current mode</dt><dd><StatusPill value={item.exposure} /></dd></div>
            <div><dt>Requested mode</dt><dd><StatusPill value={mode} /></dd></div>
          </dl>
          {mode === "public" && (
            <div className="danger-zone">
              <strong>This endpoint will be reachable from the public internet.</strong>
              <p>Confirm the application&apos;s own authentication is appropriate. Type the exact hostname to continue.</p>
              <label>Type <span className="mono">{item.hostname}</span>
                <input value={confirmation} onChange={(event) => setConfirmation(event.currentTarget.value)} autoComplete="off" spellCheck={false} />
              </label>
            </div>
          )}
          <MutationNotice error={mutation.error} />
          <div className="modal-actions">
            <button type="button" className="button secondary" onClick={onClose}>Cancel</button>
            <button type="button" className={mode === "public" ? "button danger" : "button primary"} disabled={!publicConfirmed || mutation.isPending || mode === item.exposure} onClick={() => mutation.mutate()}>
              {mutation.isPending ? "Applying…" : "Apply exposure"}
            </button>
          </div>
        </div>
    </dialog>
  );
}
// Bundles without a browser interface (no app_path in deploy/catalog/catalog.json).
// The backend omits launch_url for these even when a hostname is stored
// (e.g. Caddy's hostname points at the dashboard, not a Caddy UI).
const INFRASTRUCTURE_BUNDLES: Record<string, true> = { caddy: true, "embedding-worker": true, "restic-server": true };

export function ApplicationsPage() {
  const { client } = useAuth();
  const queryClient = useQueryClient();
  const toast = useToast();
  const query = useQuery({ queryKey: ["applications"], queryFn: client.applications });
  const [review, setReview] = useState<Application | null>(null);
  const [pendingAction, setPendingAction] = useState<{ id: string; action: "stop" | "update"; hostname: string; name: string } | null>(null);
  const mutation = useMutation({
    mutationFn: ({ id, action }: { id: string; action: "start" | "stop" | "restart" | "update" }) => client.applicationAction(id, action),
    onSuccess: (_data, vars) => {
      queryClient.invalidateQueries({ queryKey: ["applications"] });
      const label = vars.action === "stop" ? "Application stopped" : vars.action === "update" ? "Application update started" : vars.action === "restart" ? "Application restarted" : "Application started";
      toast.success(label);
      setPendingAction(null);
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Application action failed"),
  });
  const applications = query.data ?? [];

  const runningCount = applications.filter((a) => a.observed_state === "running").length;
  const attentionCount = applications.filter((a) => a.health === "unhealthy" || a.health === "degraded").length;

  return (
    <div className="page">
      <PageHeader title="Applications" />
      {query.isLoading ? <LoadingState label="Loading applications" /> : query.isError ? <ErrorState error={query.error} retry={() => void query.refetch()} /> : !applications.length ? (
        <EmptyState title="No applications" description="No platform services are currently reported." />
      ) : (
        <>
          <p className="app-statusline" role="status">
            $ omahab apps list — <strong>{applications.length} svcs</strong> · {runningCount} running · {attentionCount} need attention
          </p>
          <div className="table-wrap">
            <table className="app-table">
              <thead>
                <tr><th scope="col">app</th><th scope="col">health</th><th scope="col">exposure</th><th scope="col">route</th><th scope="col">version</th><th scope="col">state</th><th scope="col">updated</th><th scope="col"><span className="sr-only">Actions</span></th></tr>
              </thead>
              <tbody>
                {applications.map((application) => {
                  const running = application.observed_state === "running";
                  const url = application.launch_url || null;
                  const infrastructure = INFRASTRUCTURE_BUNDLES[application.bundle_id] === true;
                  return (
                    <tr key={application.id}>
                      <td className="app-name">{url ? <a href={url} target="_blank" rel="noreferrer" title={url}>{application.name}</a> : application.name}</td>
                      <td><StatusPill value={application.health} /></td>
                      <td><StatusPill value={application.exposure} /></td>
                      <td>{url ? <>{url} <CopyButton text={url} label="Copy launch URL" /></> : infrastructure ? "No web interface" : application.hostname ? <>{application.hostname} <CopyButton text={application.hostname} label="Copy" /></> : "—"}</td>
                      <td title={application.digest}>{shortDigest(application.digest)} <CopyButton text={application.digest} label="Copy digest" /></td>
                      <td>{application.desired_state}→{application.observed_state}</td>
                      <td>{formatDate(application.updated_at)}</td>
                      <td className="cell-actions">
                        <div className="row-actions">
                          <button className="button secondary" type="button" disabled={mutation.isPending} onClick={() => mutation.mutate({ id: application.id, action: running ? "restart" : "start" })}>{running ? "restart" : "start"}</button>
                          {running && <button className="button ghost" type="button" disabled={mutation.isPending} onClick={() => setPendingAction({ id: application.id, action: "stop", hostname: application.hostname || application.image, name: application.name })}>stop</button>}
                          <button className="button secondary" type="button" onClick={() => setReview(application)}>exposure</button>
                        </div>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
          <MutationNotice error={mutation.error} />
        </>
      )}
      {review && <ExposureReview resource="applications" item={review} onClose={() => setReview(null)} />}
      {pendingAction && (
        <DestructiveConfirm
          title={`${pendingAction.action === "stop" ? "Stop" : "Update"} ${pendingAction.name}`}
          description={pendingAction.action === "stop" ? "The application will be stopped. Existing sessions may be interrupted." : "The application will update to the latest pinned digest. The previous version will be retained until the new one is healthy."}
          confirmValue={pendingAction.hostname}
          confirmLabel={`Type ${pendingAction.hostname === pendingAction.name ? "the application image" : "the hostname"}`}
          onClose={() => setPendingAction(null)}
          onConfirm={() => mutation.mutate({ id: pendingAction.id, action: pendingAction.action })}
          pending={mutation.isPending}
          error={mutation.error}
        />
      )}
    </div>
  );
}

function ProjectReleases({ project }: { project: Project }) {
  const { client } = useAuth();
  const queryClient = useQueryClient();
  const toast = useToast();
  const query = useQuery({ queryKey: ["projects", project.id, "releases"], queryFn: () => client.releases(project.id) });
  const rollback = useMutation({
    mutationFn: (releaseId: string) => client.rollbackRelease(project.id, releaseId),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["projects", project.id, "releases"] });
      toast.success("Release rollback started");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Rollback failed"),
  });
  const [confirmRelease, setConfirmRelease] = useState<Release | null>(null);
  const releases = query.data ?? [];
  if (query.isLoading) return <LoadingState label="Loading releases" />;
  if (query.isError) return <ErrorState error={query.error} retry={() => void query.refetch()} />;
  if (!releases.length) return <EmptyState title="No releases" description="Releases appear here after a successful project build." />;
  return (
    <div className="compact-list">
      {releases.map((release) => (
        <div key={release.id}>
          <div>
            <strong className="mono">{release.commit.slice(0, 12)}</strong> <CopyButton text={release.commit} label="Copy" />
            {release.digest && <small className="mono">{shortDigest(release.digest)} <CopyButton text={release.digest} label="Copy digest" /></small>}
            <span>{formatDate(release.created_at)}</span>
          </div>
          <StatusPill value={release.active ? "active" : release.status} />
          {!release.active && <button className="button ghost" type="button" disabled={rollback.isPending} onClick={() => setConfirmRelease(release)}>Roll back to this</button>}
        </div>
      ))}
      <MutationNotice error={rollback.error} />
      {confirmRelease && (
        <DestructiveConfirm
          title="Roll back release"
          description={`Roll back ${project.name} to ${confirmRelease.commit.slice(0, 12)}? The current active release will be replaced.`}
          confirmValue={confirmRelease.commit.slice(0, 12)}
          confirmLabel="Type the commit prefix"
          onClose={() => setConfirmRelease(null)}
          onConfirm={() => { rollback.mutate(confirmRelease.id); setConfirmRelease(null); }}
          pending={rollback.isPending}
          error={rollback.error}
        />
      )}
    </div>
  );
}

export function ProjectsPage() {
  const { client } = useAuth();
  const { pathname } = useLocation();
  const adminPrefix = pathname.startsWith("/admin") ? "/admin" : "";
  const queryClient = useQueryClient();
  const toast = useToast();
  const query = useQuery({ queryKey: ["projects"], queryFn: client.projects });
  const instanceQuery = useQuery({ queryKey: ["instance"], queryFn: client.instance });
  const [review, setReview] = useState<Project | null>(null);
  const [showForm, setShowForm] = useState(false);
  const [name, setName] = useState("");
  const [slug, setSlug] = useState("");
  const [formError, setFormError] = useState<string | null>(null);
  const projects = query.data ?? [];
  const domain = (instanceQuery.data?.domain ?? "").trim().toLowerCase();
  const domainReady = domain !== "" && domain !== "example.com" && domain !== "not-configured.invalid";
  const creationBlocked = instanceQuery.isSuccess && !domainReady;
  const create = useMutation({
    mutationFn: (input: { name: string; slug?: string }) => client.createProject(input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["projects"] });
      toast.success("Project created");
      setName("");
      setSlug("");
      setFormError(null);
      setShowForm(false);
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Could not create project"),
  });
  function openForm() {
    setFormError(null);
    setShowForm(true);
  }
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const trimmedName = name.trim();
    const trimmedSlug = slug.trim();
    if (!trimmedName) {
      setFormError("Name is required.");
      return;
    }
    if (trimmedName.length > 100) {
      setFormError("Name must be 100 characters or fewer.");
      return;
    }
    if (trimmedSlug && !/^[a-z0-9-]+$/.test(trimmedSlug)) {
      setFormError("Slug may only contain lowercase letters, numbers, and hyphens.");
      return;
    }
    if (trimmedSlug.length > 63) {
      setFormError("Slug must be 63 characters or fewer.");
      return;
    }
    setFormError(null);
    create.mutate(trimmedSlug ? { name: trimmedName, slug: trimmedSlug } : { name: trimmedName });
  }
  return (
    <div className="page">
      <PageHeader title="Projects" actions={<button className="button primary" type="button" disabled={creationBlocked || create.isPending} onClick={openForm}>New project</button>} />
      {showForm && (
        <form className="form-stack" aria-label="New project" onSubmit={submit}>
          {instanceQuery.isLoading ? <LoadingState label="Checking setup" /> : instanceQuery.isError ? <ErrorState error={instanceQuery.error} retry={() => void instanceQuery.refetch()} /> : !domainReady ? <p className="muted">Set up a domain before creating projects. <Link to={`${adminPrefix}/setup`}>Continue setup</Link></p> : (
            <>
              <label>Name<input value={name} onChange={(e) => setName(e.target.value)} required maxLength={100} disabled={create.isPending} /></label>
              <label>Slug (optional)<input value={slug} onChange={(e) => setSlug(e.target.value)} maxLength={63} placeholder="Derived from name" disabled={create.isPending} /></label>
              <div className="row-actions">
                <button className="button primary" type="submit" disabled={create.isPending}>{create.isPending ? "Creating…" : "Create project"}</button>
                <button className="button secondary" type="button" disabled={create.isPending} onClick={() => { setShowForm(false); setFormError(null); }}>Cancel</button>
              </div>
            </>
          )}
          {formError ? <p className="inline-error" role="alert">{formError}</p> : null}
          <MutationNotice error={create.error} />
        </form>
      )}
      {query.isLoading ? <LoadingState label="Loading projects" /> : query.isError ? <ErrorState error={query.error} retry={() => void query.refetch()} /> : !projects.length ? <EmptyState title="No projects" description="Create a project to connect a Forgejo repository and deployment pipeline." action={<div className="form-stack"><button className="button primary" type="button" disabled={creationBlocked || create.isPending} onClick={openForm}>New project</button>{creationBlocked ? <p className="muted">Set up a domain before creating projects. <Link to={`${adminPrefix}/setup`}>Continue setup</Link></p> : null}</div>} /> : (
        <div className="table-wrap"><table><thead><tr><th scope="col">Project</th><th scope="col">Repository</th><th scope="col">Host</th><th scope="col">Exposure</th><th scope="col"><span className="sr-only">Actions</span></th></tr></thead><tbody>{projects.map((project) => <Fragment key={project.id}><tr><td><strong>{project.name}</strong></td><td className="cell-wrap">{project.repository_url}</td><td className="cell-wrap">{project.hostname} <CopyButton text={project.hostname} label="Copy" /></td><td><StatusPill value={project.exposure} /></td><td className="cell-actions"><button className="button secondary" type="button" onClick={() => setReview(project)}>Exposure</button></td></tr><tr className="detail-row"><td colSpan={5}><details><summary>Releases</summary><ProjectReleases project={project} /></details></td></tr></Fragment>)}</tbody></table></div>
      )}
      {review && <ExposureReview resource="projects" item={review} onClose={() => setReview(null)} />}
    </div>
  );
}

export function BackupsPage() {
  const { client } = useAuth();
  const queryClient = useQueryClient();
  const toast = useToast();
  const query = useQuery({ queryKey: ["backups"], queryFn: client.backups });
  const create = useMutation({
    mutationFn: client.createBackup,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["backups"] });
      toast.success("Backup started");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Backup failed"),
  });
  const verify = useMutation({
    mutationFn: client.verifyBackup,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["backups"] });
      toast.success("Restore verification started");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Verification failed"),
  });
  const backups = query.data ?? [];
  return (
    <div className="page">
      <PageHeader title="Backups" actions={<button className="button primary" type="button" disabled={create.isPending} onClick={() => create.mutate()}>{create.isPending ? "Starting…" : "Back up now"}</button>} />
      <MutationNotice error={create.error ?? verify.error} />
      {query.isLoading ? <LoadingState label="Loading backup history" /> : query.isError ? <ErrorState error={query.error} retry={() => void query.refetch()} /> : !backups.length ? <EmptyState title="No backup history" description="Start an encrypted backup, then run restore verification before relying on it." action={<button className="button primary" type="button" disabled={create.isPending} onClick={() => create.mutate()}>{create.isPending ? "Starting…" : "Back up now"}</button>} /> : (
        <div className="table-wrap"><table><thead><tr><th>Status</th><th>Snapshot</th><th>Started</th><th>Restore verification</th><th><span className="sr-only">Actions</span></th></tr></thead><tbody>{backups.map((backup) => <tr key={backup.id}><td><StatusPill value={backup.status} />{backup.error && <span className="cell-error">{backup.error}</span>}</td><td className="mono">{backup.snapshot_id ? <><span>{shortDigest(backup.snapshot_id)}</span> <CopyButton text={backup.snapshot_id} label="Copy" /></> : "—"}</td><td>{formatDate(backup.started_at)}</td><td>{backup.verified_at ? <><StatusPill value="verified" /><small>{formatDate(backup.verified_at)}</small></> : <StatusPill value="not verified" />}</td><td><button className="button secondary" type="button" disabled={!backup.snapshot_id || verify.isPending} onClick={() => verify.mutate(backup.id)}>Verify restore</button></td></tr>)}</tbody></table></div>
      )}
    </div>
  );
}

export function EventsPage() {
  const { client } = useAuth();
  const queryClient = useQueryClient();
  const toast = useToast();
  const query = useQuery({ queryKey: ["events"], queryFn: client.events });
  const ntfyQuery = useQuery({ queryKey: ["ntfy"], queryFn: client.ntfyConfig });
  const read = useMutation({
    mutationFn: client.markEventRead,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["events"] });
      toast.success("Event marked read");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Could not mark read"),
  });
  const markAll = useMutation({
    mutationFn: client.markAllEventsRead,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["events"] });
      toast.success("All events marked read");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Could not mark all read"),
  });
  const toggleNtfy = useMutation({
    mutationFn: (enabled: boolean) => client.setNtfyEnabled(enabled),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["ntfy"] });
      toast.success("Phone notifications updated");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Could not update ntfy"),
  });
  const grouped = useMemo(() => query.data?.slice().sort((left, right) => Date.parse(right.created_at) - Date.parse(left.created_at)), [query.data]);
  const unreadCount = query.data?.filter((event) => !event.read_at).length ?? 0;
  const ntfy = ntfyQuery.data;
  const topic = ntfy?.topic ?? "";
  const ntfyUrl = topic ? `http://${typeof window !== "undefined" ? window.location.hostname : "omahab"}:2586/${topic}` : "";
  return (
    <div className="page">
      <PageHeader title="Events" actions={<button className="button secondary" type="button" disabled={markAll.isPending || unreadCount === 0} onClick={() => markAll.mutate()}>{markAll.isPending ? "Marking…" : "Mark all read"}</button>} />
      <Section title="Phone notifications" description="Send warning and error notifications to your phone.">
        {ntfyQuery.isLoading ? <LoadingState label="Loading ntfy" /> : ntfyQuery.isError ? <ErrorState error={ntfyQuery.error} retry={() => void ntfyQuery.refetch()} /> : (
          <div style={{ display: "flex", gap: "1rem", alignItems: "flex-start", flexWrap: "wrap" }}>
            <div style={{ flex: 1, minWidth: "260px" }}>
              <label className="check-row" style={{ gap: "0.5rem", display: "flex", alignItems: "center" }}>
                <input type="checkbox" checked={!!ntfy?.enabled} disabled={toggleNtfy.isPending} onChange={(e) => toggleNtfy.mutate(e.target.checked)} />
                <span>Enable phone notifications (warning+ to ntfy)</span>
              </label>
              {ntfy?.enabled && topic ? (
                <div style={{ marginTop: "0.75rem" }}>
                  <p style={{ margin: 0 }}>Topic: <code className="mono">{topic}</code> <CopyButton text={topic} label="Copy topic" /></p>
                  <p className="muted" style={{ fontSize: "0.85rem", margin: "0.25rem 0" }}>Server posts to <code className="mono">http://127.0.0.1:2586/{topic}</code> for severities <code>warning|error</code>. Add this topic in the ntfy app (server URL <code>http://{typeof window !== "undefined" ? window.location.hostname : "host"}:2586</code>).</p>
                  <p className="muted" style={{ fontSize: "0.85rem" }}>QR encodes the ntfy URL for quick phone pairing.</p>
                  {topic && <div style={{ display: "flex", gap: "0.5rem", alignItems: "center", marginTop: "0.5rem" }}><CopyButton text={ntfyUrl} label="Copy ntfy URL" /><a href={ntfyUrl} target="_blank" rel="noreferrer" className="button secondary">Open ntfy</a></div>}
                </div>
              ) : (
                <p className="muted" style={{ fontSize: "0.85rem", marginTop: "0.5rem" }}>Phone notifications are off. Enable to generate a random 24-char topic and start posting warnings/errors to the local ntfy.</p>
              )}
              {toggleNtfy.error ? <p className="inline-error" role="alert">{toggleNtfy.error instanceof Error ? toggleNtfy.error.message : "Update failed"}</p> : null}
            </div>
            {ntfy?.enabled && topic && (
              <div style={{ display: "grid", placeItems: "center", gap: "0.25rem" }}>
                <QRCode value={ntfyUrl} label="ntfy topic QR" />
                <small className="muted" style={{ display: "block", textAlign: "center" }}>Scan to subscribe</small>
              </div>
            )}
          </div>
        )}
      </Section>
      {query.isLoading ? <LoadingState label="Loading events" /> : query.isError ? <ErrorState error={query.error} retry={() => void query.refetch()} /> : !grouped?.length ? <EmptyState title="Inbox is clear" description="New operational events will appear here as they happen." /> : (
        <ol className="event-list">{grouped.map((event) => <li key={event.id} className={event.read_at ? "read" : "unread"}><span className="event-time">{formatDate(event.created_at)}</span><StatusPill value={event.severity} /><div className="event-message"><strong>{event.message}</strong><p>{event.type.replaceAll(".", " · ")}</p></div>{!event.read_at && <button className="button ghost" type="button" disabled={read.isPending} onClick={() => read.mutate(event.id)}>Mark read</button>}</li>)}</ol>
      )}
      <MutationNotice error={read.error ?? markAll.error} />
    </div>
  );
}
