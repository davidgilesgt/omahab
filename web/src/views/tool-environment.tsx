import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "../auth";
import { EmptyState, ErrorState, formatDate, LoadingState, PageHeader, Section } from "../components/ui";
import { useToast } from "../components/toast";
import { CopyButton } from "../components/copyButton";

const RESERVED_NAMES: Record<string, true> = {
  OPENAI_BASE_URL: true,
  OPENAI_API_KEY: true,
  ANTHROPIC_BASE_URL: true,
  ANTHROPIC_API_KEY: true,
  OMAHAB_MODEL_FAST: true,
  OMAHAB_MODEL_BALANCED: true,
  OMAHAB_MODEL_REASONING: true,
  OMAHAB_MODEL_EMBEDDING: true,
};

const NAME_RE = /^[A-Z_][A-Z0-9_]{0,127}$/;

function validateName(name: string): string | null {
  if (!NAME_RE.test(name)) return "Name must match ^[A-Z_][A-Z0-9_]{0,127}$";
  if (RESERVED_NAMES[name]) return `${name} is reserved (per-device model key/URL) and cannot be overwritten`;
  return null;
}

function validateValue(value: string): string | null {
  if (value.includes("\0")) return "Value must not contain NUL";
  if (value.includes("\r") || value.includes("\n")) return "Value must not contain CR/LF (one-line systemd assignment required)";
  return null;
}

export function ToolEnvironmentPage() {
  const { client } = useAuth();
  const queryClient = useQueryClient();
  const toast = useToast();
  const varsQuery = useQuery({ queryKey: ["tool-environment"], queryFn: client.toolEnvironment });
  const [enrollCode, setEnrollCode] = useState<string | null>(null);
  const [enrollExpires, setEnrollExpires] = useState<string | null>(null);

  const putVar = useMutation({
    mutationFn: ({ name, value }: { name: string; value: string }) => client.putToolVariable(name, value),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["tool-environment"] });
      toast.success("Variable saved — devices will sync on next environment.changed");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Save failed"),
  });

  const deleteVar = useMutation({
    mutationFn: (name: string) => client.deleteToolVariable(name),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["tool-environment"] });
      toast.success("Variable deleted");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Delete failed"),
  });

  const enroll = useMutation({
    mutationFn: client.createCompanionEnrollment,
    onSuccess: (data) => {
      setEnrollCode(data.code);
      setEnrollExpires(data.expires_at);
      toast.success("Enrollment code created — single-use, 10 min");
      queryClient.invalidateQueries({ queryKey: ["companion-devices"] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Enrollment failed"),
  });

  function submitVar(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    const name = String(form.get("name") ?? "").trim();
    const value = String(form.get("value") ?? "");
    const nameErr = validateName(name);
    if (nameErr) {
      toast.error(nameErr);
      return;
    }
    const valErr = validateValue(value);
    if (valErr) {
      toast.error(valErr);
      return;
    }
    if (!value) {
      toast.error("Value required (write-only)");
      return;
    }
    putVar.mutate({ name, value }, { onSuccess: () => (event.target as HTMLFormElement).reset() });
  }

  const variables = varsQuery.data ?? [];

  return (
    <div className="page">
      <PageHeader eyebrow="Agent environment" title="Tool environment" description="Server-authoritative synchronized environment agent-tools. Devices never upload; every mutation bumps one revision. Reserved model keys/URLs are composed per device at fetch. See Devices for enrolled companions." />
      <div className="split-grid wide-primary">
        <Section title="Variables" description="Names/versions only — values are write-only and never returned. Browser never calls the device environment endpoint.">
          {varsQuery.isLoading ? <LoadingState label="Loading variables" /> : varsQuery.isError ? <ErrorState error={varsQuery.error} retry={() => void varsQuery.refetch()} /> : !variables.length ? <EmptyState title="No variables" description="Create a variable; it will be delivered to granted companion devices on next sync." /> : (
            <div className="compact-list">
              {variables.map((v) => (
                <div key={v.name} style={{ display: "flex", justifyContent: "space-between", alignItems: "center", padding: "0.5rem 0", borderBottom: "1px solid var(--border)" }}>
                  <div>
                    <strong className="mono">{v.name}</strong> <span className="muted">v{v.version}</span>
                    <div className="muted" style={{ fontSize: "0.8rem" }}>updated {formatDate(v.updated_at)}</div>
                  </div>
                  <button className="button secondary" type="button" disabled={deleteVar.isPending} onClick={() => deleteVar.mutate(v.name)}>Delete</button>
                </div>
              ))}
            </div>
          )}
          <form className="form-stack" onSubmit={submitVar} style={{ marginTop: "1rem" }}>
            <h4 style={{ margin: 0 }}>Create or rotate variable</h4>
            <label>Name <input name="name" required autoComplete="off" placeholder="PARALLEL_API_KEY" pattern="^[A-Z_][A-Z0-9_]{0,127}$" /></label>
            <label>Value <input name="value" type="password" required autoComplete="new-password" /></label>
            <small className="muted">Validates ^[A-Z_][A-Z0-9_]{"{0,127}"}$, rejects NUL/CR/LF and reserved names {Object.keys(RESERVED_NAMES).join(", ")}.</small>
            <button className="button primary" type="submit" disabled={putVar.isPending}>{putVar.isPending ? "Saving…" : "Save variable"}</button>
            {putVar.error ? <p className="inline-error" role="alert">{putVar.error instanceof Error ? putVar.error.message : "Save failed"}</p> : null}
            {deleteVar.error ? <p className="inline-error" role="alert">{deleteVar.error instanceof Error ? deleteVar.error.message : "Delete failed"}</p> : null}
          </form>
        </Section>
        <Section title="Companion enrollment" description="Enrollment codes are 192 random bits, single-use, 10-minute expiry, shown once. Device tokens are oma_dev_… stored only as hash.">
          <div className="form-stack">
            <button className="button primary" type="button" disabled={enroll.isPending} onClick={() => enroll.mutate()}>Create enrollment code</button>
            {enroll.error ? <p className="inline-error" role="alert">{enroll.error instanceof Error ? enroll.error.message : "Enrollment failed"}</p> : null}
            {enrollCode && (
              <div className="recovery-banner" role="status">
                <div>
                  <strong>Enrollment code — copy now (single-use)</strong>
                  <p><code className="mono">{enrollCode}</code> <CopyButton text={enrollCode} label="Copy" /></p>
                  {enrollExpires && <small>Expires {formatDate(enrollExpires)} (10m)</small>}
                  <p><small className="muted">Device runs <code>omahab-clientd enroll</code> with hidden prompt; token stored only in Secret Service (go-keyring service omahab, account device-token). Per-device Forgejo token <code>device-&lt;id&gt;</code> and restic credentials are issued automatically.</small></p>
                </div>
                <button className="icon-button" type="button" onClick={() => setEnrollCode(null)} aria-label="Dismiss">×</button>
              </div>
            )}
            {enrollCode && (
              <div className="callout" role="status" style={{ border: "1px solid var(--border)", borderRadius: "0.5rem", padding: "0.75rem", background: "var(--surface-raised, #f8f8f4)" }}>
                <strong>One-liner (Omarchy) — paste on the device</strong>
                <pre className="mono" style={{ whiteSpace: "pre-wrap", wordBreak: "break-all", margin: "0.5rem 0", padding: "0.5rem", background: "var(--surface)", borderRadius: "0.25rem", fontSize: "0.85rem" }}>{`curl -fsSL ${typeof window !== "undefined" ? window.location.origin : ""}/install.sh?code=${encodeURIComponent(enrollCode)} | sh`}</pre>
                <div style={{ display: "flex", gap: "0.5rem", alignItems: "center", flexWrap: "wrap" }}>
                  <CopyButton text={`curl -fsSL ${typeof window !== "undefined" ? window.location.origin : ""}/install.sh?code=${encodeURIComponent(enrollCode)} | sh`} label="Copy one-liner" />
                  <small className="muted">Installs binary to ~/.local/bin, unit to ~/.config/systemd/user/omahab-clientd.service (ExecStart %h/.local/bin/omahab-clientd), Quickshell plugin to Omarchy plugin dir, then enrolls. Code single-use, 10m.</small>
                </div>
              </div>
            )}
            <small className="muted">If Secret Service is unavailable, enrollment/sync fails with diagnostic — never falls back to plaintext.</small>
            <p className="muted" style={{ fontSize: "0.85rem" }}><a href="/devices">View Devices →</a> to see enrolled companions, version skew, env revision, and PC backup age.</p>
          </div>
        </Section>
      </div>
    </div>
  );
}
