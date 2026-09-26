import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "../auth";
import { EmptyState, ErrorState, formatDate, LoadingState, PageHeader, Section } from "../components/ui";
import { useToast } from "../components/toast";
import { CopyButton } from "../components/copyButton";

function shellQuote(s: string): string {
  return "'" + s.replace(/'/g, "'\\''") + "'";
}

function validatedOrigin(): string {
  if (typeof window === "undefined") return "";
  try {
    const o = window.location.origin;
    if (!o) return "";
    const u = new URL(o);
    if (u.protocol !== "http:" && u.protocol !== "https:") return "";
    if (!u.host) return "";
    return u.origin;
  } catch {
    return "";
  }
}

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
  const devicesQuery = useQuery({ queryKey: ["companion-devices"], queryFn: client.companionDevices });
  const [enrollCode, setEnrollCode] = useState<string | null>(null);
  const [enrollExpires, setEnrollExpires] = useState<string | null>(null);

  const grantEnv = useMutation({
    mutationFn: client.grantDeviceEnv,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["companion-devices"] });
      toast.success("Environment shared — device receives current values on next sync");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Share failed"),
  });
  const revokeEnv = useMutation({
    mutationFn: client.revokeDeviceEnvGrant,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["companion-devices"] });
      toast.success("Environment unshared — device bundle now empty");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Unshare failed"),
  });
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
      <PageHeader title="Tool environment" description="Values are write-only and never returned. Devices never upload; every mutation bumps one revision." />
      <div className="split-grid wide-primary">
        <Section title="Variables" description="Names/versions only — values are write-only and never returned. Browser never calls the device environment endpoint.">
          {varsQuery.isLoading ? <LoadingState label="Loading variables" /> : varsQuery.isError ? <ErrorState error={varsQuery.error} retry={() => void varsQuery.refetch()} /> : !variables.length ? <EmptyState title="No variables" description="Create a variable; it will be delivered to granted companion devices on next sync." /> : (
            <div className="compact-list">
              {variables.map((v) => (
                <div key={v.name} style={{ display: "flex", justifyContent: "space-between", alignItems: "center", padding: "0.5rem 0", borderBottom: "var(--border) solid var(--line)" }}>
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
                  <p><small className="muted">Paste this at the hidden enrollment prompt after running the installer. Device runs <code>omahab-clientd enroll</code> with hidden prompt; token stored only in Secret Service (go-keyring service omahab, account device-token). Per-device Forgejo token <code>device-&lt;id&gt;</code> and restic credentials are issued automatically.</small></p>
                </div>
                <button className="icon-button" type="button" onClick={() => setEnrollCode(null)} aria-label="Dismiss">×</button>
              </div>
            )}
            {enrollCode && (() => {
              const origin = validatedOrigin();
              const quotedOrigin = origin ? shellQuote(origin) : "''";
              const quotedInstall = origin ? shellQuote(origin + "/install.sh") : "'/install.sh'";
              const oneLiner = `curl -fsSL ${quotedInstall} | OMAHAB_SERVER=${quotedOrigin} sh`;
              return (
                <div className="callout" role="status">
                  <strong>One-liner (Omarchy) — paste on the device</strong>
                  <pre className="mono">{oneLiner}</pre>
                  <div style={{ display: "flex", gap: "0.5rem", alignItems: "center", flexWrap: "wrap" }}>
                    <CopyButton text={oneLiner} label="Copy one-liner" />
                    <small className="muted">Installs binary to ~/.local/bin, unit to ~/.config/systemd/user/omahab-clientd.service (ExecStart %h/.local/bin/omahab-clientd), Quickshell plugin to Omarchy plugin dir, then prompts for the enrollment code. Code single-use, 10m. Paste the code at the hidden prompt.</small>
                  </div>
                  <p><small className="muted">Plain HTTP on untrusted networks cannot protect script integrity or the enrollment POST. Use an encrypted tunnel, Tailnet, or HTTPS on untrusted networks.</small></p>
                </div>
              );
            })()}
            <small className="muted">If Secret Service is unavailable, enrollment/sync fails with diagnostic — never falls back to plaintext.</small>
            <p className="muted" style={{ fontSize: "0.85rem" }}><a href="/devices">View Devices →</a> to see enrolled companions, version skew, env revision, and PC backup age.</p>
          </div>
        </Section>
      </div>
      <Section title="Device access" description="Explicit owner consent per device — names and consent state only. Values are write-only and never shown here. Sharing is never automatic: enrollment alone grants nothing.">
        {devicesQuery.isLoading ? <LoadingState label="Loading devices" /> : devicesQuery.isError ? <ErrorState error={devicesQuery.error} retry={() => void devicesQuery.refetch()} /> : !(devicesQuery.data ?? []).length ? <EmptyState title="No companion devices" description="Create an enrollment code above, then run the one-liner on the device. After enrollment, share the environment explicitly per device." /> : (
          <div className="compact-list">
            {(devicesQuery.data ?? []).map((d) => (
              <div key={d.id} style={{ display: "flex", justifyContent: "space-between", alignItems: "center", padding: "0.5rem 0", borderBottom: "var(--border) solid var(--line)" }}>
                <div>
                  <strong>{d.name || d.id}</strong> {d.hostname ? <span className="muted">· {d.hostname}</span> : null}
                  <div className="muted" style={{ fontSize: "0.8rem" }}>{d.granted ? "shared — receives current values on next sync, rotations included" : "not shared — environment fetch returns empty"}</div>
                </div>
                {d.granted ? (
                  <button className="button secondary" type="button" disabled={revokeEnv.isPending} onClick={() => revokeEnv.mutate(d.id)}>Unshare</button>
                ) : (
                  <button className="button secondary" type="button" disabled={grantEnv.isPending} onClick={() => grantEnv.mutate(d.id)}>Share</button>
                )}
              </div>
            ))}
          </div>
        )}
        {grantEnv.error ? <p className="inline-error" role="alert">{grantEnv.error instanceof Error ? grantEnv.error.message : "Share failed"}</p> : null}
        {revokeEnv.error ? <p className="inline-error" role="alert">{revokeEnv.error instanceof Error ? revokeEnv.error.message : "Unshare failed"}</p> : null}
      </Section>
    </div>
  );
}
