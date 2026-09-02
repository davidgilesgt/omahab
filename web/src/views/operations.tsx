import { useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "../auth";
import type { Application, Backup, Exposure, Project, Release } from "../api/types";
import { EmptyState, ErrorState, formatDate, LoadingState, PageHeader, Section, shortDigest, StatusPill } from "../components/ui";
import { useToast } from "../components/toast";
import { CopyButton } from "../components/copyButton";

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

export function OverviewPage() {
  const { client } = useAuth();
  const status = useQuery({ queryKey: ["status"], queryFn: client.status, refetchInterval: 30_000 });
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
  return (
    <div className="page">
      {setup.data && setup.data.state !== "complete" && (
        <div className="banner-card" style={{ background: "var(--warning-muted, #fff3cd)", border: "1px solid var(--warning, #f59e0b)", padding: 12, borderRadius: 8, marginBottom: 16 }}>
          <strong>Setup is not finished</strong> — <Link to="/setup">Continue setup</Link>
        </div>
      )}
      <PageHeader eyebrow="At a glance" title="Your server" description="Health, recovery readiness, and changes that need attention." />
      <div className="metric-strip">
        <article><span>Control plane</span><strong><StatusPill value={status.data.health} /></strong><small>Version {status.data.version}</small></article>
        <article><span>Applications</span><strong>{applications.data?.length ?? "—"}</strong><small>{unhealthy.length ? `${unhealthy.length} need attention` : "No reported issues"}</small></article>
        <article><span>Unread events</span><strong>{events.data ? unread.length : "—"}</strong><small>Operational inbox</small></article>
        <article><span>Verified recovery</span><strong>{latestBackup?.verified_at ? "Current" : "Not verified"}</strong><small>{latestBackup?.verified_at ? formatDate(latestBackup.verified_at) : "Run a restore verification"}</small></article>
      </div>
      <div className="split-grid">
        <Section title="Needs attention" description="Degraded services and unread high-priority events.">
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
        <Section title="Recovery posture" description="A backup is healthy only after a successful restore verification.">
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
        <header><div><p className="eyebrow">Review change</p><h2 id="exposure-title">Exposure for {item.name}</h2></div><button type="button" className="icon-button" onClick={onClose} aria-label="Close">×</button></header>
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

export function ApplicationsPage() {
  const { client } = useAuth();
  const queryClient = useQueryClient();
  const toast = useToast();
  const query = useQuery({ queryKey: ["applications"], queryFn: client.applications });
  const catalogQuery = useQuery({ queryKey: ["catalog"], queryFn: client.catalog });
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
  const install = useMutation({
    mutationFn: (bundleId: string) => client.installApplication({ bundle_id: bundleId }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["applications"] });
      queryClient.invalidateQueries({ queryKey: ["catalog"] });
      toast.success("Application installed");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Install failed"),
  });
  const applications = query.data ?? [];
  // Native (systemd-runtime) bundles are adopted automatically by the
  // setup reconciler; they never show an Install button.
  const catalog = (catalogQuery.data ?? []).filter((bundle) => !bundle.installed && bundle.runtime !== "systemd");

  return (
    <div className="page">
      <PageHeader eyebrow="Catalog" title="Applications" description="Install curated, digest-pinned bundles and operate them without editing generated runtime definitions." />
      {catalogQuery.data && catalog.length > 0 && (
        <div className="resource-list">
          {catalog.map((bundle) => (
            <article className="resource-row" key={bundle.id}>
              <div className="resource-main"><div className="resource-title"><h2>{bundle.name}</h2><StatusPill value={bundle.default_exposure} /></div><p className="mono">{bundle.image} <CopyButton text={bundle.image} label="Copy" /></p><small>{bundle.architectures.join(" / ")}{bundle.memory_mb ? ` · ~${bundle.memory_mb} MiB` : ""} · max exposure {bundle.max_exposure}</small></div>
              <div className="row-actions">
                <button className="button primary" type="button" disabled={install.isPending} onClick={() => install.mutate(bundle.id)}>{install.isPending ? "Installing…" : "Install"}</button>
              </div>
            </article>
          ))}
          <MutationNotice error={install.error} />
        </div>
      )}
      {query.isLoading ? <LoadingState label="Loading applications" /> : query.isError ? <ErrorState error={query.error} retry={() => void query.refetch()} /> : !applications.length ? (
        <EmptyState title="Nothing installed" description={catalogQuery.data && !catalog.length ? "Every curated bundle is already installed." : "Install a curated bundle above to see it here."} />
      ) : (
        <div className="resource-list">
          {applications.map((application) => {
            const nativeBundle = (catalogQuery.data ?? []).some((b) => b.id === application.bundle_id && b.runtime === "systemd");
            const running = application.observed_state === "running";
            return (
              <article className="resource-row" key={application.id}>
                <div className="resource-main"><div className="resource-title"><h2>{application.name}</h2><StatusPill value={application.health} /><StatusPill value={application.exposure} /></div><p className="mono">{application.hostname || application.image} <CopyButton text={application.hostname || application.image} label="Copy" /></p><small>Digest <span className="mono">{shortDigest(application.digest)}</span> <CopyButton text={application.digest} label="Copy digest" /></small><small>Desired {application.desired_state} · observed {application.observed_state} · updated {formatDate(application.updated_at)}</small></div>
                <div className="row-actions">
                  <button className="button secondary" type="button" disabled={mutation.isPending} onClick={() => mutation.mutate({ id: application.id, action: running ? "restart" : "start" })}>{running ? "Restart" : "Start"}</button>
                  {running && <button className="button ghost" type="button" disabled={mutation.isPending} onClick={() => setPendingAction({ id: application.id, action: "stop", hostname: application.hostname || application.image, name: application.name })}>Stop</button>}
                  {!nativeBundle && <button className="button ghost" type="button" disabled={mutation.isPending} onClick={() => setPendingAction({ id: application.id, action: "update", hostname: application.hostname || application.image, name: application.name })}>Update</button>}
                  <button className="button secondary" type="button" onClick={() => setReview(application)}>Exposure</button>
                </div>
              </article>
            );
          })}
          <MutationNotice error={mutation.error} />
        </div>
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
  const query = useQuery({ queryKey: ["projects"], queryFn: client.projects });
  const [review, setReview] = useState<Project | null>(null);
  const projects = query.data ?? [];
  const createCommand = "omahab project create --name my-app --repo https://forge.example.com/owner/repo";
  return (
    <div className="page">
      <PageHeader eyebrow="Build & deploy" title="Projects and releases" description="Inspect immutable releases and deliberately select what is active." />
      {query.isLoading ? <LoadingState label="Loading projects" /> : query.isError ? <ErrorState error={query.error} retry={() => void query.refetch()} /> : !projects.length ? <EmptyState title="No projects" description="Create a project with the CLI to connect a Forgejo repository and deployment pipeline." action={<div className="form-stack"><code className="mono">{createCommand}</code><CopyButton text={createCommand} label="Copy command" /></div>} /> : (
        <div className="resource-list">{projects.map((project) => <article className="resource-row project-row" key={project.id}><div className="resource-main"><div className="resource-title"><h2>{project.name}</h2><StatusPill value={project.exposure} /></div><p>{project.repository_url}</p><small className="mono">{project.hostname} <CopyButton text={project.hostname} label="Copy" /></small><details><summary>Releases</summary><ProjectReleases project={project} /></details></div><div className="row-actions"><button className="button secondary" type="button" onClick={() => setReview(project)}>Exposure</button></div></article>)}</div>
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
      <PageHeader eyebrow="Recovery" title="Backups" description="Encrypted snapshots and evidence that they can actually be restored." actions={<button className="button primary" type="button" disabled={create.isPending} onClick={() => create.mutate()}>{create.isPending ? "Starting…" : "Back up now"}</button>} />
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
  const read = useMutation({
    mutationFn: client.markEventRead,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["events"] });
      toast.success("Event marked read");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Could not mark read"),
  });
  const grouped = useMemo(() => query.data?.slice().sort((left, right) => Date.parse(right.created_at) - Date.parse(left.created_at)), [query.data]);
  return (
    <div className="page">
      <PageHeader eyebrow="Operational inbox" title="Events" description="A live, durable record of health changes and actions across your server." />
      {query.isLoading ? <LoadingState label="Loading events" /> : query.isError ? <ErrorState error={query.error} retry={() => void query.refetch()} /> : !grouped?.length ? <EmptyState title="Inbox is clear" description="New operational events will appear here as they happen." /> : (
        <ol className="event-list">{grouped.map((event) => <li key={event.id} className={event.read_at ? "read" : "unread"}><span className="event-dot" aria-hidden="true" /><div><div className="resource-title"><StatusPill value={event.severity} /><strong>{event.message}</strong></div><p>{event.type.replaceAll(".", " · ")}</p><small>{formatDate(event.created_at)}</small></div>{!event.read_at && <button className="button ghost" type="button" disabled={read.isPending} onClick={() => read.mutate(event.id)}>Mark read</button>}</li>)}</ol>
      )}
      <MutationNotice error={read.error} />
    </div>
  );
}
