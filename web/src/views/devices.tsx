import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "../auth";
import { EmptyState, ErrorState, formatDate, LoadingState, PageHeader, Section, StatusPill } from "../components/ui";
import { useToast } from "../components/toast";
import { CopyButton } from "../components/copyButton";

function formatRelative(value?: string | null) {
  if (!value) return "never";
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return value;
  const diffMs = Date.now() - parsed.getTime();
  const mins = Math.round(diffMs / 60000);
  if (mins < 1) return "just now";
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.round(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.round(hours / 24);
  return `${days}d ago`;
}

function VersionSkew({ clientVersion, serverVersion }: { clientVersion?: string | null; serverVersion?: string | null }) {
  if (!clientVersion) return <span className="muted">unknown</span>;
  if (!serverVersion) return <span className="mono">{clientVersion}</span>;
  const skew = clientVersion !== serverVersion;
  return (
    <span style={{ display: "inline-flex", gap: "0.35rem", alignItems: "center" }}>
      <code className="mono">{clientVersion}</code>
      {skew && <span className="status status-warning" style={{ fontSize: "0.7rem", padding: "0.15rem 0.35rem" }}>update available</span>}
      {!skew && <span className="status status-positive" style={{ fontSize: "0.7rem", padding: "0.15rem 0.35rem" }}>up to date</span>}
    </span>
  );
}

export function DevicesPage() {
  const { client } = useAuth();
  const queryClient = useQueryClient();
  const toast = useToast();
  const devicesQuery = useQuery({ queryKey: ["companion-devices"], queryFn: client.companionDevices });
  const statusQuery = useQuery({ queryKey: ["status"], queryFn: client.status });
  const [revokeId, setRevokeId] = useState<string | null>(null);

  const revoke = useMutation({
    mutationFn: client.revokeCompanionDevice,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["companion-devices"] });
      toast.success("Device revoked");
      setRevokeId(null);
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Revoke failed"),
  });

  const serverVersion = statusQuery.data?.version ?? null;
  const devices = devicesQuery.data ?? [];

  return (
    <div className="page">
      <PageHeader eyebrow="Companion" title="Devices" description="Enrolled companion devices — version, last seen, environment, and PC backup. Revoke per-device; Forgejo access is per-device revocable." />
      {devicesQuery.isLoading ? (
        <LoadingState label="Loading devices" />
      ) : devicesQuery.isError ? (
        <ErrorState error={devicesQuery.error} retry={() => void devicesQuery.refetch()} />
      ) : !devices.length ? (
        <EmptyState title="No companion devices" description="Enroll an Omarchy device from Tool environment → Create enrollment code, then run the one-liner on the device." />
      ) : (
        <Section title="Enrolled devices" description="Last seen, client version vs server, env revision, and PC backup age. Per-device Forgejo tokens are revoked on device revocation.">
          <div className="compact-list">
            {devices.map((d) => {
              const lastSeen = d.last_seen_at ?? d.last_sync_at ?? d.updated_at;
              const backupAge = d.backup_last_snapshot ? formatRelative(d.backup_last_snapshot) : "never";
              return (
                <div key={d.id} style={{ display: "flex", gap: "1rem", justifyContent: "space-between", alignItems: "flex-start", padding: "0.75rem 0", borderBottom: "var(--border) solid var(--line)" }}>
                  <div style={{ flex: 1, minWidth: 0 }}>
                    <div style={{ display: "flex", gap: "0.5rem", alignItems: "center", flexWrap: "wrap" }}>
                      <strong>{d.name || d.id}</strong>
                      <code className="mono" style={{ fontSize: "0.75rem" }}>{d.device_token_prefix ?? d.id.slice(0, 8)}…</code>
                      <StatusPill value={d.revoked_at ? "revoked" : "active"} />
                    </div>
                    <div className="muted" style={{ fontSize: "0.8rem", marginTop: "0.15rem" }}>
                      {d.hostname ? `${d.hostname} · ` : ""}
                      {d.platform ? `${d.platform}/${d.arch ?? ""}` : ""}
                      {d.shell ? ` · ${d.shell}` : ""}
                    </div>
                    <div style={{ display: "flex", gap: "0.75rem", flexWrap: "wrap", marginTop: "0.35rem", fontSize: "0.85rem" }}>
                      <span>
                        Last seen: <strong>{formatRelative(lastSeen)}</strong> <span className="muted">({formatDate(lastSeen)})</span>
                      </span>
                      <span>
                        Client: <VersionSkew clientVersion={d.client_version} serverVersion={serverVersion} />
                      </span>
                      <span>
                        Env: <code className="mono">rev {d.env_revision}</code> <span className="muted">({d.env_variable_count} vars)</span>
                      </span>
                      <span>
                        PC backup: <strong>{backupAge}</strong> {d.backup_last_snapshot ? <span className="muted">({formatDate(d.backup_last_snapshot)})</span> : <span className="muted">— run backup on device</span>}
                      </span>
                    </div>
                    <div className="muted" style={{ fontSize: "0.8rem", marginTop: "0.2rem" }}>created {formatDate(d.created_at)} · updated {formatDate(d.updated_at)}</div>
                  </div>
                  <div style={{ display: "flex", flexDirection: "column", gap: "0.5rem", minWidth: "140px", alignItems: "flex-end" }}>
                    <button className="button danger" type="button" disabled={revoke.isPending} onClick={() => setRevokeId(d.id)}>
                      Revoke
                    </button>
                    {d.hostname && <CopyButton text={d.hostname} label="Copy hostname" />}
                  </div>
                </div>
              );
            })}
          </div>
          <p className="muted" style={{ fontSize: "0.85rem", marginTop: "0.75rem" }}>
            Server version: <code className="mono">{serverVersion ?? "unknown"}</code> · Devices report <code className="mono">clientd_version</code> via daily <code className="mono">PUT /api/v1/companion/devices/me</code> (hostname, platform, arch, shell, last_seen). Revoking deletes its <code className="mono">device-&lt;id&gt;</code> Forgejo token and LiteLLM key.
          </p>
        </Section>
      )}

      {revokeId && (
        <dialog className="modal" open aria-labelledby="revoke-title">
          <header>
            <div>
              <p className="eyebrow">Confirm</p>
              <h2 id="revoke-title">Revoke device</h2>
            </div>
            <button type="button" className="icon-button" onClick={() => setRevokeId(null)} aria-label="Close">
              ×
            </button>
          </header>
          <div className="form-stack">
            <p>Revoke this companion device? Its LiteLLM key and <code>device-&lt;id&gt;</code> Forgejo token will be revoked immediately. Already-downloaded shared keys remain on the device until you rotate them.</p>
            <div className="modal-actions">
              <button type="button" className="button secondary" onClick={() => setRevokeId(null)}>
                Cancel
              </button>
              <button type="button" className="button danger" disabled={revoke.isPending} onClick={() => revoke.mutate(revokeId)}>
                {revoke.isPending ? "Revoking…" : "Revoke device"}
              </button>
            </div>
            {revoke.error ? <p className="inline-error" role="alert">{revoke.error instanceof Error ? revoke.error.message : "Revoke failed"}</p> : null}
          </div>
        </dialog>
      )}
    </div>
  );
}
