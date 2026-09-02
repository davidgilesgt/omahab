import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "../auth";
import type { SetupCheck, SetupStatus } from "../api/types";
import { ErrorState, LoadingState, PageHeader, Section, StatusPill } from "../components/ui";
import { useToast } from "../components/toast";
import { CopyButton } from "../components/copyButton";

function authHeaders(): Record<string, string> {
  const t = sessionStorage.getItem("omahab.session") ?? "";
  return t ? { Authorization: `Bearer ${t}` } : {};
}

function CheckRow({ check }: { check: SetupCheck }) {
  const showAction = check.owner === "operator" && (check.status === "pending" || check.status === "failed") && check.action;
  return (
    <li style={{ display: "flex", gap: 8, alignItems: "flex-start", flexDirection: "column" }}>
      <div style={{ display: "flex", gap: 8, alignItems: "center", width: "100%" }}>
        <StatusPill value={check.status} />
        <strong>{check.label}</strong>
      </div>
      {check.detail && <span style={{ opacity: 0.7, fontSize: "0.9em" }}>{check.detail}</span>}
      {showAction && <span style={{ fontSize: "0.9em" }}>{check.action}</span>}
      {check.id === "core_apps" && check.apps && check.apps.length > 0 && (
        <ul style={{ marginLeft: 16, listStyle: "none", padding: 0, width: "100%" }}>
          {check.apps.map((app) => (
            <li key={app.bundle_id} style={{ display: "flex", gap: 8, alignItems: "center", padding: "4px 0" }}>
              <StatusPill value={app.status} />
              <span>{app.bundle_id}</span>
            </li>
          ))}
        </ul>
      )}
      {check.id === "admin_passkeys" && typeof check.passkey_count === "number" && (
        <span style={{ marginLeft: 16, opacity: 0.8 }}>
          {check.passkey_count}/{check.target ?? 2} passkeys registered
        </span>
      )}
    </li>
  );
}

function Checklist({ setup }: { setup: SetupStatus }) {
  const operator = setup.checks.filter((c) => c.owner === "operator");
  const system = setup.checks.filter((c) => c.owner === "system");
  return (
    <>
      <Section title="Your steps" description="Work only you can complete.">
        <ul className="activity-list">
          {operator.map((check) => (
            <CheckRow key={check.id} check={check} />
          ))}
        </ul>
      </Section>
      <Section title="Omahab sets up automatically" description="DNS, certificates, and core services.">
        <ul className="activity-list">
          {system.map((check) => (
            <CheckRow key={check.id} check={check} />
          ))}
        </ul>
      </Section>
    </>
  );
}

export function SetupPage() {
  const { client } = useAuth();
  const toast = useToast();
  const queryClient = useQueryClient();

  const setupQuery = useQuery({
    queryKey: ["setup"],
    queryFn: client.setup,
    refetchInterval: (query) => {
      const data = query.state.data as SetupStatus | undefined;
      if (data && data.state === "reconciling") return 5000;
      // also poll when waiting or attention? spec says poll while reconciling only
      return false;
    },
  });

  const usersQuery = useQuery({ queryKey: ["users"], queryFn: client.users });
  const instanceQuery = useQuery({ queryKey: ["instance"], queryFn: client.instance });

  const [domain, setDomain] = useState("");
  const [dnsToken, setDnsToken] = useState("");
  const [tunnelToken, setTunnelToken] = useState("");
  const [zoneId, setZoneId] = useState("");
  const [accountId, setAccountId] = useState("");

  const [inviteName, setInviteName] = useState("");
  const [inviteEmail, setInviteEmail] = useState("");
  const [enrollmentUrl, setEnrollmentUrl] = useState<string | null>(null);
  const [enrollmentExpires, setEnrollmentExpires] = useState<string | null>(null);

  const [woodpeckerUsername, setWoodpeckerUsername] = useState("");
  const [woodpeckerToken, setWoodpeckerToken] = useState("");

  const reconcileMutation = useMutation({
    mutationFn: client.reconcileSetup,
    onSuccess: () => {
      toast.success("Automatic setup started");
      void queryClient.invalidateQueries({ queryKey: ["setup"] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Reconciliation failed"),
  });

  const cloudflareMutation = useMutation({
    mutationFn: async () => {
      if (!domain.trim()) throw new Error("Domain is required");
      await client.updateInstance({ domain: domain.trim() });
      const secrets: Array<{ name: string; value: string }> = [];
      if (dnsToken.trim()) secrets.push({ name: "cloudflare_dns", value: dnsToken.trim() });
      if (tunnelToken.trim()) secrets.push({ name: "cloudflare_tunnel", value: tunnelToken.trim() });
      if (zoneId.trim()) secrets.push({ name: "cloudflare_zone_id", value: zoneId.trim() });
      if (accountId.trim()) secrets.push({ name: "cloudflare_account_id", value: accountId.trim() });
      for (const s of secrets) {
        await client.createSecret({ scope: "platform-app", name: s.name, value: s.value });
      }
      await client.reconcileSetup();
    },
    onSuccess: () => {
      toast.success("Cloudflare configuration saved, reconciliation started");
      void queryClient.invalidateQueries({ queryKey: ["setup"] });
      void queryClient.invalidateQueries({ queryKey: ["instance"] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Save failed"),
  });

  const inviteMutation = useMutation({
    mutationFn: async () => {
      if (!inviteEmail.trim() || !inviteName.trim()) throw new Error("Name and email required");
      const user = await client.createUser({ name: inviteName.trim(), email: inviteEmail.trim(), groups: ["admins"] });
      return user;
    },
    onSuccess: (user) => {
      if (user.enrollment_url) {
        setEnrollmentUrl(user.enrollment_url);
        setEnrollmentExpires(user.enrollment_expires_at ?? null);
        toast.success("Invite created");
      } else {
        toast.error("Pocket ID is not ready — wait until core apps include pocket-id, then use Get enrollment link");
      }
      void queryClient.invalidateQueries({ queryKey: ["users"] });
      void queryClient.invalidateQueries({ queryKey: ["setup"] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Invite failed"),
  });

  const enrollmentMutation = useMutation({
    mutationFn: async () => {
      const user = usersQuery.data?.[0];
      if (!user) throw new Error("No user");
      const updated = await client.issueEnrollment(user.id);
      return updated;
    },
    onSuccess: (user) => {
      if (user.enrollment_url) {
        setEnrollmentUrl(user.enrollment_url);
        setEnrollmentExpires(user.enrollment_expires_at ?? null);
        toast.success("Enrollment link ready");
      }
      void queryClient.invalidateQueries({ queryKey: ["users"] });
      void queryClient.invalidateQueries({ queryKey: ["setup"] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Enrollment failed"),
  });

  const [recovery, setRecovery] = useState<{ public_key: string; private_key: string; kit: string } | null>(null);
  const [recoverySaved, setRecoverySaved] = useState(false);
  const [repoLabel, setRepoLabel] = useState("");
  const [repoLocation, setRepoLocation] = useState("");
  const [repoPassword, setRepoPassword] = useState("");

  const verifyTokenMutation = useMutation({
    mutationFn: async () => {
      const res = await fetch("/api/v1/setup/verify-cloudflare", {
        method: "POST",
        headers: { "Content-Type": "application/json", ...authHeaders() },
        body: JSON.stringify({ token: dnsToken.trim() }),
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data?.error?.message ?? `HTTP ${res.status}`);
      return data as { ok: boolean; status?: string; detail?: string };
    },
    onSuccess: (data) => {
      if (data.ok) toast.success(`Token active (${data.status ?? "active"})`);
      else toast.error(`Token check failed: ${data.detail ?? data.status ?? "rejected"}`);
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Verification failed"),
  });

  const recoveryGenerateMutation = useMutation({
    mutationFn: async () => {
      const res = await fetch("/api/v1/recovery/generate", {
        method: "POST",
        headers: { ...authHeaders() },
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data?.error?.message ?? `HTTP ${res.status}`);
      return data as { public_key: string; private_key: string; kit: string };
    },
    onSuccess: (data) => {
      setRecovery(data);
      setRecoverySaved(false);
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Generation failed"),
  });

  const recoveryConfirmMutation = useMutation({
    mutationFn: async () => {
      const res = await fetch("/api/v1/recovery/confirm", {
        method: "POST",
        headers: { "Content-Type": "application/json", ...authHeaders() },
        body: JSON.stringify({ public_key: recovery?.public_key }),
      });
      if (!res.ok) {
        const data = await res.json().catch(() => ({}));
        throw new Error(data?.error?.message ?? `HTTP ${res.status}`);
      }
    },
    onSuccess: () => {
      setRecoverySaved(true);
      toast.success("Recovery kit saved to the server");
      void queryClient.invalidateQueries({ queryKey: ["setup"] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Confirm failed"),
  });

  const repoMutation = useMutation({
    mutationFn: async () => {
      const res = await fetch("/api/v1/backup-repositories", {
        method: "POST",
        headers: { "Content-Type": "application/json", ...authHeaders() },
        body: JSON.stringify({
          label: repoLabel.trim(),
          location: repoLocation.trim(),
          password: repoPassword,
        }),
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data?.error?.message ?? `HTTP ${res.status}`);
      return data;
    },
    onSuccess: () => {
      toast.success("Backup repository configured; daily backup timer enabled");
      setRepoPassword("");
      void queryClient.invalidateQueries({ queryKey: ["setup"] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Configure failed"),
  });

  const woodpeckerMutation = useMutation({
    mutationFn: async () => {
      if (!woodpeckerUsername.trim() || !woodpeckerToken.trim()) throw new Error("Username and token required");
      return client.setupWoodpecker({ username: woodpeckerUsername.trim(), token: woodpeckerToken.trim() });
    },
    onSuccess: () => {
      setWoodpeckerToken("");
      toast.success("Woodpecker connected");
      void queryClient.invalidateQueries({ queryKey: ["setup"] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Woodpecker connect failed"),
  });


  if (setupQuery.isLoading) return <LoadingState label="Loading setup status" />;
  if (setupQuery.isError) return <ErrorState error={setupQuery.error} retry={() => void setupQuery.refetch()} />;
  const setup = setupQuery.data;
  if (!setup) return <LoadingState label="Loading setup status" />;

  const hasUsers = (usersQuery.data?.length ?? 0) > 0;
  const instanceDomain = instanceQuery.data?.domain ?? "";
  const sshHost = instanceDomain || (typeof window !== "undefined" ? window.location.hostname : "your-server");
  const recoveryEmail = usersQuery.data?.[0]?.email ?? inviteEmail ?? "admin@example.com";

  // Find passkey count from checks
  const adminCheck = setup.checks.find((c) => c.id === "admin_passkeys");
  const passkeyCount = adminCheck?.passkey_count ?? 0;
  const passkeyTarget = adminCheck?.target ?? 2;
  const coreAppsCheck = setup.checks.find((c) => c.id === "core_apps");
  const isCoreAppsOk = coreAppsCheck?.status === "ok";
  const isIdentityNotConfigured = adminCheck?.detail?.includes("identity not configured");
  const inviteDisabled = inviteMutation.isPending || !isCoreAppsOk || !!isIdentityNotConfigured;
  const woodpeckerCheck = setup.checks.find((c) => c.id === "woodpecker_connection");
  const isWoodpeckerSectionVisible =
    !!woodpeckerCheck || (isCoreAppsOk && !!instanceDomain && instanceDomain !== "example.com" && instanceDomain !== "not-configured.invalid");

  return (
    <div className="page">
      <PageHeader
        eyebrow="Setup"
        title="Continue setup"
        description="Complete Cloudflare, app provisioning, and passkey enrollment."
        actions={
          <button className="button secondary" type="button" onClick={() => void reconcileMutation.mutate()} disabled={reconcileMutation.isPending}>
            {reconcileMutation.isPending ? "Retrying…" : "Retry automatic setup"}
          </button>
        }
      />

      {setup.state === "waiting_for_cloudflare" && (
        <Section title="Cloudflare" description="Enter your domain and scoped API tokens to enable DNS and tunnel.">
          <div className="form-stack">
            <label className="field">
              <span>Domain</span>
              <input value={domain} onChange={(e) => setDomain(e.target.value)} placeholder="example.com" />
            </label>
            <label className="field">
              <span>Token A (DNS, Zone:Read — cloudflare_dns)</span>
              <input type="password" value={dnsToken} onChange={(e) => setDnsToken(e.target.value)} placeholder="dns token" />
            </label>
            <label className="field">
              <span>Token B (Tunnel — cloudflare_tunnel)</span>
              <input type="password" value={tunnelToken} onChange={(e) => setTunnelToken(e.target.value)} placeholder="tunnel token (optional)" />
            </label>
            <label className="field">
              <span>Zone ID (optional — cloudflare_zone_id)</span>
              <input value={zoneId} onChange={(e) => setZoneId(e.target.value)} placeholder="zone id" />
            </label>
            <label className="field">
              <span>Account ID (optional — cloudflare_account_id)</span>
              <input value={accountId} onChange={(e) => setAccountId(e.target.value)} placeholder="account id" />
            </label>
            <div style={{ display: "flex", gap: 8 }}>
              <button className="button secondary" type="button" onClick={() => void verifyTokenMutation.mutate()} disabled={verifyTokenMutation.isPending || !dnsToken.trim()}>
                {verifyTokenMutation.isPending ? "Verifying…" : "Verify DNS token"}
              </button>
              <button className="button primary" type="button" onClick={() => void cloudflareMutation.mutate()} disabled={cloudflareMutation.isPending}>
                {cloudflareMutation.isPending ? "Saving…" : "Save and reconcile"}
              </button>
            </div>
            {cloudflareMutation.isError && (
              <p className="inline-error" role="alert">{cloudflareMutation.error instanceof Error ? cloudflareMutation.error.message : "Save failed"}</p>
            )}
          </div>
        </Section>
      )}

      {instanceDomain && instanceDomain !== "example.com" && instanceDomain !== "not-configured.invalid" && (
        <Section title="Service addresses" description="Canonical application URLs.">
          <p>
            <a href={`https://id.${instanceDomain}`} target="_blank" rel="noreferrer">
              {`https://id.${instanceDomain}`}
            </a>
          </p>
          <p>
            Use this address for identity setup. id.home.{instanceDomain} is a DNS routing record, not a website.
          </p>
        </Section>
      )}

      <Checklist setup={setup} />

      {isWoodpeckerSectionVisible && (
        <Section title="Connect Woodpecker" description="Authorize Woodpecker CI with Forgejo using a personal access token.">
          <div className="form-stack">
            <p>
              Woodpecker authenticates through Forgejo. Ensure{" "}
              <a href={`https://git.${instanceDomain}`} target="_blank" rel="noreferrer">
                {`https://git.${instanceDomain}`}
              </a>{" "}
              and{" "}
              <a href={`https://ci.${instanceDomain}`} target="_blank" rel="noreferrer">
                {`https://ci.${instanceDomain}`}
              </a>{" "}
              are reachable, then sign in to{" "}
              <a href={`https://ci.${instanceDomain}`} target="_blank" rel="noreferrer">
                {`https://ci.${instanceDomain}`}
              </a>{" "}
              via Pocket ID → Forgejo and copy the token from Woodpecker’s CLI &amp; API settings.
            </p>
            {woodpeckerCheck && (
              <p style={{ display: "flex", gap: 8, alignItems: "center", flexWrap: "wrap" }}>
                Status: <StatusPill value={woodpeckerCheck.status} />
                {woodpeckerCheck.detail && <span style={{ opacity: 0.7, fontSize: "0.9em" }}>{woodpeckerCheck.detail}</span>}
              </p>
            )}
            <label className="field">
              <span>Forgejo username</span>
              <input
                value={woodpeckerUsername}
                onChange={(e) => setWoodpeckerUsername(e.target.value)}
                placeholder="forgejo username"
                autoComplete="username"
              />
            </label>
            <label className="field">
              <span>Woodpecker PAT</span>
              <input
                type="password"
                value={woodpeckerToken}
                onChange={(e) => setWoodpeckerToken(e.target.value)}
                placeholder="woodpecker token"
                autoComplete="off"
              />
            </label>
            <button
              className="button primary"
              type="button"
              onClick={() => void woodpeckerMutation.mutate()}
              disabled={woodpeckerMutation.isPending || !woodpeckerUsername.trim() || !woodpeckerToken.trim()}
            >
              {woodpeckerMutation.isPending ? "Connecting…" : "Connect Woodpecker"}
            </button>
            {woodpeckerMutation.isError && (
              <p className="inline-error" role="alert">
                {woodpeckerMutation.error instanceof Error ? woodpeckerMutation.error.message : "Connect failed"}
              </p>
            )}
            {woodpeckerMutation.isSuccess && (
              <p style={{ color: "var(--success, #15803d)", fontSize: "0.9em" }}>Woodpecker connected. Token cleared and not displayed.</p>
            )}
          </div>
        </Section>
      )}

      <Section title="Admin passkeys" description="Invite an admin and register passkeys via Pocket ID.">
        {!hasUsers ? (
          <div className="form-stack">
            <p>No admin user yet. Invite the first administrator:</p>
            <label className="field">
              <span>Name</span>
              <input value={inviteName} onChange={(e) => setInviteName(e.target.value)} placeholder="Alice" />
            </label>
            <label className="field">
              <span>Email</span>
              <input type="email" value={inviteEmail} onChange={(e) => setInviteEmail(e.target.value)} placeholder="alice@example.com" />
            </label>
            <button className="button primary" type="button" onClick={() => void inviteMutation.mutate()} disabled={inviteDisabled}>
              {inviteMutation.isPending ? "Inviting…" : "Create admin and get enrollment link"}
            </button>
            {(!isCoreAppsOk || isIdentityNotConfigured) && (
              <p className="inline-error" role="alert">Complete core apps and Pocket ID setup first — check the checklist above.</p>
            )}
            {inviteMutation.isError && (
              <p className="inline-error" role="alert">{inviteMutation.error instanceof Error ? inviteMutation.error.message : "Invite failed"}</p>
            )}
          </div>
        ) : (
          <div>
            <p>{usersQuery.data?.length ?? 0} user(s) registered.</p>
            <button className="button primary" type="button" onClick={() => void enrollmentMutation.mutate()} disabled={enrollmentMutation.isPending}>
              {enrollmentMutation.isPending ? "Getting link…" : "Get enrollment link"}
            </button>
            {enrollmentMutation.isError && (
              <p className="inline-error" role="alert">{enrollmentMutation.error instanceof Error ? enrollmentMutation.error.message : "Enrollment failed"}</p>
            )}
          </div>
        )}

        {enrollmentUrl && (
          <div style={{ marginTop: 12, padding: 12, border: "1px solid var(--border, #ddd)", borderRadius: 6 }}>
            <p>
              <strong>Enrollment link:</strong> <a href={enrollmentUrl} target="_blank" rel="noreferrer">{enrollmentUrl}</a>{" "}
              <CopyButton text={enrollmentUrl} label="Copy" />
            </p>
            {enrollmentExpires && <small>Expires {enrollmentExpires}</small>}
            <p style={{ marginTop: 8 }}><small>Open this link in the admin’s browser to register a passkey on Pocket ID.</small></p>
          </div>
        )}

        <div style={{ marginTop: 12 }}>
          <span>Passkeys: {passkeyCount}/{passkeyTarget}</span>
          <button className="button secondary" type="button" style={{ marginLeft: 8 }} onClick={() => { void setupQuery.refetch(); void usersQuery.refetch(); }}>
            Refresh
          </button>
        </div>
      </Section>

      <Section title="Recovery key" description="Generate an age key pair; the private key and kit are shown once and never stored server-side.">
        {!recovery ? (
          <div className="form-stack">
            <p>Generate a recovery key pair now — you must save both the private key and the recovery kit offline.</p>
            <button className="button primary" type="button" onClick={() => void recoveryGenerateMutation.mutate()} disabled={recoveryGenerateMutation.isPending}>
              {recoveryGenerateMutation.isPending ? "Generating…" : "Generate recovery key"}
            </button>
            {recoveryGenerateMutation.isError && (
              <p className="inline-error" role="alert">{recoveryGenerateMutation.error instanceof Error ? recoveryGenerateMutation.error.message : "Generation failed"}</p>
            )}
          </div>
        ) : (
          <div className="form-stack">
            <div>
              <strong>Private key (save this offline — shown once):</strong>
              <div style={{ display: "flex", gap: 8, alignItems: "center", background: "var(--surface-muted, #f6f6f6)", padding: 8, borderRadius: 6, marginTop: 4 }}>
                <code style={{ flex: 1, wordBreak: "break-all" }} className="mono">{recovery.private_key}</code>
                <CopyButton text={recovery.private_key} label="Copy" />
              </div>
            </div>
            <div>
              <strong>Recovery kit (armored):</strong>
              <div style={{ display: "flex", gap: 8, background: "var(--surface-muted, #f6f6f6)", padding: 8, borderRadius: 6, marginTop: 4 }}>
                <pre style={{ flex: 1, whiteSpace: "pre-wrap", wordBreak: "break-all", margin: 0, fontSize: "0.8em" }} className="mono">{recovery.kit}</pre>
                <CopyButton text={recovery.kit} label="Copy" />
              </div>
            </div>
            <label style={{ display: "flex", gap: 8, alignItems: "center" }}>
              <input type="checkbox" checked={recoverySaved} onChange={(e) => setRecoverySaved(e.target.checked)} />
              I saved both the private key and the recovery kit
            </label>
            <button className="button primary" type="button" onClick={() => void recoveryConfirmMutation.mutate()} disabled={!recoverySaved || recoveryConfirmMutation.isPending}>
              {recoveryConfirmMutation.isPending ? "Confirming…" : "Confirm and store kit"}
            </button>
            {recoveryConfirmMutation.isError && (
              <p className="inline-error" role="alert">{recoveryConfirmMutation.error instanceof Error ? recoveryConfirmMutation.error.message : "Confirm failed"}</p>
            )}
          </div>
        )}

        <p style={{ marginTop: 12 }}>Recovery drill (run as root while you have access):</p>
        <div style={{ display: "flex", gap: 8, alignItems: "center", background: "var(--surface-muted, #f6f6f6)", padding: 8, borderRadius: 6 }}>
          <code style={{ flex: 1, wordBreak: "break-all" }}>ssh {sshHost} sudo omahab identity recover {recoveryEmail}</code>
          <CopyButton text={`ssh ${sshHost} sudo omahab identity recover ${recoveryEmail}`} label="Copy" />
        </div>
        <p style={{ marginTop: 8 }}>
          Status:{" "}
          {setup.checks.find((c) => c.id === "recovery_tested")?.status === "ok" ? (
            <StatusPill value="ok" />
          ) : (
            <StatusPill value={setup.checks.find((c) => c.id === "recovery_tested")?.status ?? "pending"} />
          )}
        </p>
      </Section>

      <Section title="Storage placement" description="Optional: dedicate a disk to media (photos) or data. Skippable — the root disk holds everything by default.">
        <p className="muted">Run on the server to list candidate disks, then assign via the API:</p>
        <div style={{ display: "flex", gap: 8, alignItems: "center", background: "var(--surface-muted, #f6f6f6)", padding: 8, borderRadius: 6 }}>
          <code style={{ flex: 1, wordBreak: "break-all" }}>curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8484/api/v1/system/disks</code>
          <CopyButton text='curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8484/api/v1/system/disks' label="Copy" />
        </div>
        <p style={{ marginTop: 8 }}><small>GET /api/v1/system/disks lists filesystems; PUT /api/v1/system/storage assigns {"{"}volume, fs_uuid{"}"}.</small></p>
      </Section>

      <Section title="Backups" description="Add a restic repository (e.g. Hetzner Storage Box or S3). Daily backup + weekly verify timers enable automatically.">
        <div className="form-stack">
          <label className="field">
            <span>Label</span>
            <input value={repoLabel} onChange={(e) => setRepoLabel(e.target.value)} placeholder="primary" />
          </label>
          <label className="field">
            <span>Location (restic URL)</span>
            <input value={repoLocation} onChange={(e) => setRepoLocation(e.target.value)} placeholder="sftp:user@host:restic-repo" className="mono" />
          </label>
          <label className="field">
            <span>Repository password</span>
            <input type="password" value={repoPassword} onChange={(e) => setRepoPassword(e.target.value)} autoComplete="new-password" />
          </label>
          <button className="button primary" type="button" onClick={() => void repoMutation.mutate()} disabled={repoMutation.isPending || !repoLabel.trim() || !repoLocation.trim() || !repoPassword}>
            {repoMutation.isPending ? "Configuring…" : "Add repository"}
          </button>
          {repoMutation.isError && (
            <p className="inline-error" role="alert">{repoMutation.error instanceof Error ? repoMutation.error.message : "Configure failed"}</p>
          )}
        </div>
      </Section>

      <Section title="Recovery drill" description="Test recovery while you have root access.">
        <p>Run on the server as root to test recovery for {recoveryEmail}:</p>
        <div style={{ display: "flex", gap: 8, alignItems: "center", background: "var(--surface-muted, #f6f6f6)", padding: 8, borderRadius: 6 }}>
          <code style={{ flex: 1, wordBreak: "break-all" }}>ssh {sshHost} sudo omahab identity recover {recoveryEmail}</code>
          <CopyButton text={`ssh ${sshHost} sudo omahab identity recover ${recoveryEmail}`} label="Copy" />
        </div>
        <p style={{ marginTop: 8 }}>
          Status:{" "}
          {setup.checks.find((c) => c.id === "recovery_tested")?.status === "ok" ? (
            <StatusPill value="ok" />
          ) : (
            <StatusPill value={setup.checks.find((c) => c.id === "recovery_tested")?.status ?? "pending"} />
          )}
        </p>
      </Section>
    </div>
  );
}
