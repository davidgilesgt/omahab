import { useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "../auth";
import type { Application, Backup, ControlEvent, Exposure, Project } from "../api/types";
import { EmptyState, ErrorState, formatDate, LoadingState, PageHeader, Section, shortDigest, StatusPill } from "../components/ui";

function MutationNotice({ error }: { error: unknown }) {
  if (!error) return null;
  return <p className="inline-error" role="alert">{error instanceof Error ? error.message : "The operation failed."}</p>;
}

export function OverviewPage() {
  const { client } = useAuth();
  const status = useQuery({ queryKey: ["status"], queryFn: client.status, refetchInterval: 30_000 });
  const applications = useQuery({ queryKey: ["applications"], queryFn: client.applications });
  const backups = useQuery({ queryKey: ["backups"], queryFn: client.backups });
  const events = useQuery({ queryKey: ["events"], queryFn: client.events });

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
              <div><dt>Snapshot</dt><dd className="mono">{latestBackup.snapshot_id ? shortDigest(latestBackup.snapshot_id) : "Pending"}</dd></div>
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
      onClose();
    },
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
            <div><dt>Resulting hostname</dt><dd className="mono">{item.hostname}</dd></div>
            <div><dt>Current mode</dt><dd><StatusPill value={item.exposure} /></dd></div>
            <div><dt>Requested mode</dt><dd><StatusPill value={mode} /></dd></div>
          </dl>
          {mode === "public" && (
            <div className="danger-zone">
              <strong>This endpoint will be reachable from the public internet.</strong>
              <p>Confirm the application’s own authentication is appropriate. Type the exact hostname to continue.</p>
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
  const query = useQuery({ queryKey: ["applications"], queryFn: client.applications });
  const catalogQuery = useQuery({ queryKey: ["catalog"], queryFn: client.catalog });
  const [review, setReview] = useState<Application | null>(null);
  const mutation = useMutation({
    mutationFn: ({ id, action }: { id: string; action: "start" | "stop" | "restart" | "update" }) => client.applicationAction(id, action),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["applications"] }),
  });
  const install = useMutation({
    mutationFn: (bundleId: string) => client.installApplication({ bundle_id: bundleId }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["applications"] });
      queryClient.invalidateQueries({ queryKey: ["catalog"] });
    },
  });
  const applications = query.data ?? [];
  const catalog = (catalogQuery.data ?? []).filter((bundle) => !bundle.installed);

  return (
    <div className="page">
      <PageHeader eyebrow="Catalog" title="Applications" description="Install curated, digest-pinned bundles and operate them without editing generated runtime definitions." />
      {catalogQuery.data && catalog.length > 0 && (
        <div className="resource-list">
          {catalog.map((bundle) => (
            <article className="resource-row" key={bundle.id}>
              <div className="resource-main"><div className="resource-title"><h2>{bundle.name}</h2><StatusPill value={bundle.default_exposure} /></div><p className="mono">{bundle.image}</p><small>{bundle.architectures.join(" / ")}{bundle.memory_mb ? ` · ~${bundle.memory_mb} MiB` : ""} · max exposure {bundle.max_exposure}</small></div>
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
            const running = application.observed_state === "running";
            return (
              <article className="resource-row" key={application.id}>
                <div className="resource-main"><div className="resource-title"><h2>{application.name}</h2><StatusPill value={application.health} /><StatusPill value={application.exposure} /></div><p className="mono">{application.hostname || application.image}</p><small>Desired {application.desired_state} · observed {application.observed_state} · updated {formatDate(application.updated_at)}</small></div>
                <div className="row-actions">
                  <button className="button secondary" type="button" disabled={mutation.isPending} onClick={() => mutation.mutate({ id: application.id, action: running ? "restart" : "start" })}>{running ? "Restart" : "Start"}</button>
                  {running && <button className="button ghost" type="button" disabled={mutation.isPending} onClick={() => mutation.mutate({ id: application.id, action: "stop" })}>Stop</button>}
                  <button className="button ghost" type="button" disabled={mutation.isPending} onClick={() => mutation.mutate({ id: application.id, action: "update" })}>Update</button>
                  <button className="button secondary" type="button" onClick={() => setReview(application)}>Exposure</button>
                </div>
              </article>
            );
          })}
          <MutationNotice error={mutation.error} />
        </div>
      )}
      {review && <ExposureReview resource="applications" item={review} onClose={() => setReview(null)} />}
    </div>
  );
}

function ProjectReleases({ project }: { project: Project }) {
  const { client } = useAuth();
  const queryClient = useQueryClient();
  const query = useQuery({ queryKey: ["projects", project.id, "releases"], queryFn: () => client.releases(project.id) });
  const rollback = useMutation({
    mutationFn: (releaseId: string) => client.rollbackRelease(project.id, releaseId),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["projects", project.id, "releases"] }),
  });
  const releases = query.data ?? [];
  if (query.isLoading) return <LoadingState label="Loading releases" />;
  if (query.isError) return <ErrorState error={query.error} retry={() => void query.refetch()} />;
  if (!releases.length) return <EmptyState title="No releases" description="Releases appear here after a successful project build." />;
  return <div className="compact-list">{releases.map((release) => <div key={release.id}><div><strong className="mono">{release.commit.slice(0, 12)}</strong><span>{formatDate(release.created_at)}</span></div><StatusPill value={release.active ? "active" : release.status} />{!release.active && <button className="button ghost" type="button" disabled={rollback.isPending} onClick={() => rollback.mutate(release.id)}>Roll back to this</button>}</div>)}<MutationNotice error={rollback.error} /></div>;
}

export function ProjectsPage() {
  const { client } = useAuth();
  const query = useQuery({ queryKey: ["projects"], queryFn: client.projects });
  const [review, setReview] = useState<Project | null>(null);
  const projects = query.data ?? [];
  return (
    <div className="page">
      <PageHeader eyebrow="Build & deploy" title="Projects and releases" description="Inspect immutable releases and deliberately select what is active." />
      {query.isLoading ? <LoadingState label="Loading projects" /> : query.isError ? <ErrorState error={query.error} retry={() => void query.refetch()} /> : !projects.length ? <EmptyState title="No projects" description="Create a project with the CLI to connect a Forgejo repository and deployment pipeline." /> : (
        <div className="resource-list">{projects.map((project) => <article className="resource-row project-row" key={project.id}><div className="resource-main"><div className="resource-title"><h2>{project.name}</h2><StatusPill value={project.exposure} /></div><p>{project.repository_url}</p><small className="mono">{project.hostname}</small><details><summary>Releases</summary><ProjectReleases project={project} /></details></div><div className="row-actions"><button className="button secondary" type="button" onClick={() => setReview(project)}>Exposure</button></div></article>)}</div>
      )}
      {review && <ExposureReview resource="projects" item={review} onClose={() => setReview(null)} />}
    </div>
  );
}

export function BackupsPage() {
  const { client } = useAuth();
  const queryClient = useQueryClient();
  const query = useQuery({ queryKey: ["backups"], queryFn: client.backups });
  const create = useMutation({ mutationFn: client.createBackup, onSuccess: () => queryClient.invalidateQueries({ queryKey: ["backups"] }) });
  const verify = useMutation({ mutationFn: client.verifyBackup, onSuccess: () => queryClient.invalidateQueries({ queryKey: ["backups"] }) });
  const backups = query.data ?? [];
  return (
    <div className="page">
      <PageHeader eyebrow="Recovery" title="Backups" description="Encrypted snapshots and evidence that they can actually be restored." actions={<button className="button primary" type="button" disabled={create.isPending} onClick={() => create.mutate()}>{create.isPending ? "Starting…" : "Back up now"}</button>} />
      <MutationNotice error={create.error ?? verify.error} />
      {query.isLoading ? <LoadingState label="Loading backup history" /> : query.isError ? <ErrorState error={query.error} retry={() => void query.refetch()} /> : !backups.length ? <EmptyState title="No backup history" description="Start an encrypted backup, then run restore verification before relying on it." /> : (
        <div className="table-wrap"><table><thead><tr><th>Status</th><th>Snapshot</th><th>Started</th><th>Restore verification</th><th><span className="sr-only">Actions</span></th></tr></thead><tbody>{backups.map((backup) => <tr key={backup.id}><td><StatusPill value={backup.status} />{backup.error && <span className="cell-error">{backup.error}</span>}</td><td className="mono">{backup.snapshot_id ? shortDigest(backup.snapshot_id) : "—"}</td><td>{formatDate(backup.started_at)}</td><td>{backup.verified_at ? <><StatusPill value="verified" /><small>{formatDate(backup.verified_at)}</small></> : <StatusPill value="not verified" />}</td><td><button className="button secondary" type="button" disabled={!backup.snapshot_id || verify.isPending} onClick={() => verify.mutate(backup.id)}>Verify restore</button></td></tr>)}</tbody></table></div>
      )}
    </div>
  );
}

export function EventsPage() {
  const { client } = useAuth();
  const queryClient = useQueryClient();
  const query = useQuery({ queryKey: ["events"], queryFn: client.events });
  const read = useMutation({ mutationFn: client.markEventRead, onSuccess: () => queryClient.invalidateQueries({ queryKey: ["events"] }) });
  const [streamError, setStreamError] = useState<string | null>(null);
  useEffect(() => {
    const controller = new AbortController();
    let retryTimer: number | undefined;
    let wakeRetry: (() => void) | null = null;
    async function maintainStream() {
      while (!controller.signal.aborted) {
        try {
          await client.streamEvents(controller.signal, (event) => {
            queryClient.setQueryData<ControlEvent[]>(["events"], (current = []) => current.some((item) => item.id === event.id) ? current : [event, ...current]);
          });
          if (!controller.signal.aborted) setStreamError("Live updates disconnected.");
        } catch (error) {
          if (!controller.signal.aborted) setStreamError(error instanceof Error ? error.message : "Live updates disconnected.");
        }
        if (controller.signal.aborted) break;
        const { promise, resolve } = Promise.withResolvers<void>();
        wakeRetry = resolve;
        retryTimer = window.setTimeout(resolve, 3_000);
        await promise;
        wakeRetry = null;
        setStreamError(null);
      }
    }
    void maintainStream();
    return () => {
      controller.abort();
      if (retryTimer !== undefined) window.clearTimeout(retryTimer);
      wakeRetry?.();
    };
  }, [client, queryClient]);
  const grouped = useMemo(() => query.data?.slice().sort((left, right) => Date.parse(right.created_at) - Date.parse(left.created_at)), [query.data]);
  return (
    <div className="page">
      <PageHeader eyebrow="Operational inbox" title="Events" description="A live, durable record of health changes and actions across your server." />
      {streamError && <p className="inline-warning" role="status">{streamError} History remains available.</p>}
      {query.isLoading ? <LoadingState label="Loading events" /> : query.isError ? <ErrorState error={query.error} retry={() => void query.refetch()} /> : !grouped?.length ? <EmptyState title="Inbox is clear" description="New operational events will appear here as they happen." /> : (
        <ol className="event-list">{grouped.map((event) => <li key={event.id} className={event.read_at ? "read" : "unread"}><span className="event-dot" aria-hidden="true" /><div><div className="resource-title"><StatusPill value={event.severity} /><strong>{event.message}</strong></div><p>{event.type.replaceAll(".", " · ")}</p><small>{formatDate(event.created_at)}</small></div>{!event.read_at && <button className="button ghost" type="button" disabled={read.isPending} onClick={() => read.mutate(event.id)}>Mark read</button>}</li>)}</ol>
      )}
      <MutationNotice error={read.error} />
    </div>
  );
}
