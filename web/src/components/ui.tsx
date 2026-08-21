import type { ReactNode } from "react";
import { ApiError } from "../api/client";

export function StatusPill({ value }: { value: string }) {
  const normalized = value.toLowerCase().replaceAll("_", "-");
  const tone = ["healthy", "running", "active", "complete", "completed", "verified", "private"].includes(normalized)
    ? "positive"
    : ["degraded", "pending", "shared", "warning"].includes(normalized)
      ? "warning"
      : ["unhealthy", "failed", "error", "public", "stopped", "disabled"].includes(normalized)
        ? "negative"
        : "neutral";
  return <span className={`status status-${tone}`}>{value.replaceAll("_", " ")}</span>;
}

export function PageHeader({ eyebrow, title, description, actions }: { eyebrow?: string; title: string; description: string; actions?: ReactNode }) {
  return (
    <header className="page-header">
      <div>
        {eyebrow && <p className="eyebrow">{eyebrow}</p>}
        <h1>{title}</h1>
        <p>{description}</p>
      </div>
      {actions && <div className="page-actions">{actions}</div>}
    </header>
  );
}

export function Section({ title, description, actions, children }: { title: string; description?: string; actions?: ReactNode; children: ReactNode }) {
  return (
    <section className="section" aria-labelledby={`section-${title.replaceAll(" ", "-").toLowerCase()}`}>
      <header className="section-header">
        <div>
          <h2 id={`section-${title.replaceAll(" ", "-").toLowerCase()}`}>{title}</h2>
          {description && <p>{description}</p>}
        </div>
        {actions && <div className="row-actions">{actions}</div>}
      </header>
      {children}
    </section>
  );
}

export function LoadingState({ label = "Loading" }: { label?: string }) {
  return <div className="state-message" role="status"><span className="spinner" aria-hidden="true" />{label}…</div>;
}

export function ErrorState({ error, retry }: { error: unknown; retry?: () => void }) {
  const message = error instanceof ApiError || error instanceof Error ? error.message : "An unexpected error occurred.";
  return (
    <div className="state-message error-state" role="alert">
      <div><strong>Couldn’t load this view</strong><p>{message}</p></div>
      {retry && <button className="button secondary" type="button" onClick={retry}>Try again</button>}
    </div>
  );
}

export function EmptyState({ title, description, action }: { title: string; description: string; action?: ReactNode }) {
  return (
    <div className="empty-state">
      <strong>{title}</strong>
      <p>{description}</p>
      {action}
    </div>
  );
}

export function formatDate(value?: string | null) {
  if (!value) return "Not yet";
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return value;
  return new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" }).format(parsed);
}

export function shortDigest(value: string) {
  return value.length > 16 ? `${value.slice(0, 9)}…${value.slice(-6)}` : value;
}
