import { useQuery } from "@tanstack/react-query";
import { useAuth } from "../auth";
import { ErrorState, LoadingState, PageHeader, Section, StatusPill } from "../components/ui";
import { useToast } from "../components/toast";

const REMEDIATION: Record<string, string> = {
  disk: "Free up disk space or expand the volume; check that the data disk is mounted.",
  services: "Check failing systemd units with systemctl status and restart them.",
  backup: "Run a backup from the Backups page and verify it can be restored.",
  backup_verified: "Run a restore verification for the latest backup.",
  tailscale: "Install Tailscale, log in, and ensure the server node is visible on your tailnet.",
  dns: "Verify the domain is registered and the home anchor DNS record points at the Tailscale IP.",
  tls: "Check Caddy and Cloudflare TLS configuration and that the hostname resolves.",
  pocketid: "Verify Pocket ID is running and reachable from the server.",
  instance: "Contact support — instance identity mismatch indicates a data restore issue.",
  encryption: "Enable disk encryption or migrate data to an encrypted volume.",
};

function remediationFor(name: string): string {
  if (REMEDIATION[name]) return REMEDIATION[name];
  if (name.startsWith("service:")) return "Check the service unit with systemctl status and journalctl -u " + name.slice(8);
  return "Check logs for this component and re-run the check.";
}

export function DoctorPage() {
  const { client } = useAuth();
  const toast = useToast();
  const query = useQuery({ queryKey: ["doctor"], queryFn: client.doctor });

  function copyMarkdown() {
    const data = query.data;
    if (!data) return;
    const lines: string[] = [
      `# Doctor report — ${new Date(data.generated_at).toISOString()}`,
      `Healthy: ${data.healthy ? "yes" : "no"}`,
      "",
      "| Check | Status | Message | Detail | Remediation |",
      "|---|---|---|---|---|",
    ];
    for (const c of data.checks) {
      const rem = remediationFor(c.name);
      const msg = (c.message ?? "").replaceAll("|", "\\|").replaceAll("\n", " ");
      const det = (c.detail ?? "").replaceAll("|", "\\|").replaceAll("\n", " ");
      lines.push(`| ${c.name} | ${c.status} | ${msg} | ${det} | ${rem} |`);
    }
    const text = lines.join("\n");
    navigator.clipboard
      .writeText(text)
      .then(() => toast.success("Doctor report copied as Markdown"))
      .catch(() => toast.error("Could not copy to clipboard"));
  }

  if (query.isLoading) return <LoadingState label="Running health checks" />;
  if (query.isError) return <ErrorState error={query.error} retry={() => void query.refetch()} />;
  if (!query.data) return <LoadingState label="Running health checks" />;

  const report = query.data;

  return (
    <div className="page">
      <PageHeader
        eyebrow="Diagnostics"
        title="Doctor"
        description="Nine probes run on the server. Healthy is quiet; degraded needs attention soon; unhealthy needs action now."
        actions={
          <>
            <button className="button secondary" type="button" onClick={() => void query.refetch()} disabled={query.isFetching}>
              {query.isFetching ? "Re-running…" : "Re-run"}
            </button>
            <button className="button ghost" type="button" onClick={copyMarkdown}>
              Copy as Markdown
            </button>
          </>
        }
      />

      <p className="muted" style={{ fontSize: "0.875rem" }}>
        Generated {new Date(report.generated_at).toLocaleString()} · Overall{" "}
        <StatusPill value={report.healthy ? "healthy" : "attention"} />
      </p>

      <Section title="Checks" description="Each row is one probe. Remediation is the next step if the status is not healthy.">
        <div className="check-list" style={{ display: "grid", gap: 12 }}>
          {report.checks.map((check) => (
            <article
              key={check.name}
              style={{
                border: "1px solid var(--line)",
                borderRadius: "var(--radius, 12px)",
                padding: 12,
                background: "var(--surface, #fff)",
                display: "grid",
                gap: 6,
              }}
            >
              <div style={{ display: "flex", gap: 8, alignItems: "center", flexWrap: "wrap" }}>
                <strong className="mono" style={{ fontSize: "0.95rem" }}>
                  {check.name}
                </strong>
                <StatusPill value={check.status} />
              </div>
              {check.message ? <p style={{ margin: 0 }}>{check.message}</p> : null}
              {check.detail ? (
                <small className="mono" style={{ color: "var(--ink-muted)", wordBreak: "break-word" }}>
                  {check.detail}
                </small>
              ) : null}
              <small style={{ color: "var(--ink-muted)" }}>
                <strong>Remediation:</strong> {remediationFor(check.name)}
              </small>
            </article>
          ))}
        </div>
      </Section>

      <Section title="About these checks" description="The same nine checks power omahab doctor --json.">
        <ul className="list">
          <li>
            <span className="mono">disk</span> — data volume usage
          </li>
          <li>
            <span className="mono">services</span> — one check per systemd unit (prefixed service:*)
          </li>
          <li>
            <span className="mono">backup</span> / <span className="mono">backup_verified</span> — last backup age and last verified restore
          </li>
          <li>
            <span className="mono">tailscale</span> — daemon installed, logged in, server node visible
          </li>
          <li>
            <span className="mono">dns</span> / <span className="mono">tls</span> — hostname resolves and TLS handshakes
          </li>
          <li>
            <span className="mono">pocketid</span> / <span className="mono">instance</span> / <span className="mono">encryption</span> — identity, instance, storage encryption
          </li>
        </ul>
      </Section>
    </div>
  );
}

export default DoctorPage;
