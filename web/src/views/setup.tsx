import { useEffect, useRef, useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "../auth";
import type { Secret, SetupStatus } from "../api/types";
import { ErrorState, LoadingState } from "../components/ui";
import { useToast } from "../components/toast";
import { CopyButton } from "../components/copyButton";

function authHeaders(): Record<string, string> {
  const t = sessionStorage.getItem("omahab.session") ?? "";
  return t ? { Authorization: `Bearer ${t}` } : {};
}

function sessionToken(): string {
  return sessionStorage.getItem("omahab.session") ?? "";
}

// Origin handoff: sessionStorage does not cross origins (LAN IP -> tailnet IP
// -> HTTPS), so carry the panel token in the fragment for auto-auth whenever
// this browser has one. On vps placement that token is what keeps the tailnet
// origin signed in. On lan placement the server trusts LAN + tailnet sources,
// so the bare URL also works and no token screen appears.
function withSessionToken(base: string): string {
  const t = sessionToken().trim();
  return t ? `${base}#token=${encodeURIComponent(t)}` : base;
}

async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // Fallback for insecure contexts: textarea selection.
  }
  try {
    const area = document.createElement("textarea");
    area.value = text;
    area.setAttribute("readonly", "");
    area.style.position = "fixed";
    area.style.opacity = "0";
    document.body.appendChild(area);
    area.select();
    let ok = false;
    try {
      ok = document.execCommand("copy");
    } catch {
      ok = false;
    }
    document.body.removeChild(area);
    return ok;
  } catch {
    return false;
  }
}

interface TailscaleStatus {
  running: boolean;
  ip: string;
  state: string;
}

async function fetchTailscaleStatus(): Promise<TailscaleStatus> {
  const res = await fetch("/api/v1/tailscale/status", { headers: { ...authHeaders() } });
  const data = (await res.json().catch(() => ({}))) as Partial<TailscaleStatus> & { error?: { message?: string } };
  if (!res.ok) throw new Error(data?.error?.message ?? `HTTP ${res.status}`);
  return { running: data.running === true, ip: typeof data.ip === "string" ? data.ip : "", state: typeof data.state === "string" ? data.state : "" };
}

async function postTailscaleUp(): Promise<{ auth_url: string }> {
  const res = await fetch("/api/v1/tailscale/up", { method: "POST", headers: { ...authHeaders() } });
  const data = (await res.json().catch(() => ({}))) as { auth_url?: unknown; error?: { message?: string } };
  if (!res.ok) throw new Error(data?.error?.message ?? `HTTP ${res.status}`);
  return { auth_url: typeof data.auth_url === "string" ? data.auth_url : "" };
}
interface NetworkStatus {
  placement: string;
  lan_closed: boolean;
  tailscale_running: boolean;
}

async function fetchNetworkStatus(): Promise<NetworkStatus> {
  const res = await fetch("/api/v1/network/status", { headers: { ...authHeaders() } });
  const data = (await res.json().catch(() => ({}))) as Partial<NetworkStatus> & { error?: { message?: string } };
  if (!res.ok) throw new Error(data?.error?.message ?? `HTTP ${res.status}`);
  return {
    placement: typeof data.placement === "string" ? data.placement : "",
    lan_closed: data.lan_closed === true,
    tailscale_running: data.tailscale_running === true,
  };
}

async function postCloseLan(): Promise<{ closed: boolean }> {
  const res = await fetch("/api/v1/network/close-lan", { method: "POST", headers: { ...authHeaders() } });
  const data = (await res.json().catch(() => ({}))) as Partial<{ closed: boolean }> & { error?: { message?: string } };
  if (!res.ok) throw new Error(data?.error?.message ?? `HTTP ${res.status}`);
  return { closed: data.closed === true };
}

async function postOpenLan(): Promise<{ closed: boolean }> {
  const res = await fetch("/api/v1/network/open-lan", { method: "POST", headers: { ...authHeaders() } });
  const data = (await res.json().catch(() => ({}))) as Partial<{ closed: boolean }> & { error?: { message?: string } };
  if (!res.ok) throw new Error(data?.error?.message ?? `HTTP ${res.status}`);
  return { closed: data.closed === true };
}
function tailnetErrorMessage(err: unknown): string {
  if (err instanceof TypeError) {
    const msg = typeof err.message === "string" ? err.message : "";
    if (msg.includes("Failed to fetch") || msg.includes("NetworkError")) {
      return "This browser cannot reach the tailnet address — check Tailscale is running on this machine, then retry.";
    }
    return msg || "Tailnet path check failed";
  }
  if (err instanceof Error) {
    return err.message;
  }
  return "Tailnet path check failed";
}

type BoxId = "ssh" | "tailscale" | "domain" | "cloudflare" | "lan" | "admin" | "recovery" | "backups" | "storage" | "woodpecker";

const BLOCKING: BoxId[] = ["ssh", "tailscale", "domain", "cloudflare", "lan", "admin", "recovery", "backups"];

function Box({
  title,
  done,
  open,
  onToggle,
  children,
}: {
  title: string;
  done: boolean;
  open: boolean;
  onToggle: () => void;
  children: React.ReactNode;
}) {
  return (
    <section className="setup-box" data-state={done ? "done" : "todo"}>
      <button type="button" className="setup-box-head" onClick={onToggle} aria-expanded={open}>
        <span className="setup-tag" data-tone={done ? "done" : "todo"}>
          {done ? "[done]" : "[todo]"}
        </span>
        <span className="setup-box-title">{title}</span>
      </button>
      {open && <div className="setup-box-body">{children}</div>}
    </section>
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
      return false;
    },
  });

  const usersQuery = useQuery({ queryKey: ["users"], queryFn: client.users });
  const instanceQuery = useQuery({ queryKey: ["instance"], queryFn: client.instance });
  const secretsQuery = useQuery<Secret[]>({
    queryKey: ["secrets", "platform-app"],
    queryFn: () => client.listSecrets("platform-app"),
  });
  const tailscaleQuery = useQuery({
    queryKey: ["tailscale-status"],
    queryFn: fetchTailscaleStatus,
    refetchInterval: (query) => {
      const data = query.state.data as TailscaleStatus | undefined;
      if (data && data.running) return false;
      return 5000;
    },
    retry: false,
  });
  const networkQuery = useQuery({
    queryKey: ["network-status"],
    queryFn: fetchNetworkStatus,
    retry: false,
  });
  // SSH keys first: the installer seeds keys, add more here.
  const sshQuery = useQuery({ queryKey: ["system-ssh-keys"], queryFn: client.systemSSHKeys, retry: false });
  const [sshGithubUser, setSshGithubUser] = useState("");
  const [sshPaste, setSshPaste] = useState("");

  const [domain, setDomain] = useState("");
  const [dnsToken, setDnsToken] = useState("");
  const [tunnelToken, setTunnelToken] = useState("");
  const [zoneId, setZoneId] = useState("");
  const [accountId, setAccountId] = useState("");

  useEffect(() => {
    const d = instanceQuery.data?.domain;
    if (d && d !== "example.com" && d !== "not-configured.invalid" && !domain) {
      setDomain(d);
    }
  }, [instanceQuery.data?.domain]);

  const existingSecrets: Record<string, true> = {};
  for (const s of secretsQuery.data ?? []) {
    existingSecrets[s.name] = true;
  }

  const [inviteName, setInviteName] = useState("");
  const [inviteEmail, setInviteEmail] = useState("");
  const [enrollmentUrl, setEnrollmentUrl] = useState<string | null>(null);
  const [enrollmentExpires, setEnrollmentExpires] = useState<string | null>(null);

  const [woodpeckerUsername, setWoodpeckerUsername] = useState("");
  const [woodpeckerToken, setWoodpeckerToken] = useState("");

  const [openOverride, setOpenOverride] = useState<BoxId | null | undefined>(undefined);
  const [coreOpen, setCoreOpen] = useState<string | null>(null);
  const advance = () => setOpenOverride(undefined);

  // Tailscale login flow: after the user starts login, prove the tailnet path
  // then hand off to the tailnet address with the session preserved in the hash.
  const [tailscaleLoginActive, setTailscaleLoginActive] = useState(false);
  const [tailscaleProving, setTailscaleProving] = useState(false);
  const tailscaleAutoTried = useRef(false);
  const tailscaleRedirected = useRef(false);

  async function proveTailscaleAndRedirect(ip: string) {
    setTailscaleProving(true);
    try {
      const res = await fetch(`http://${ip}:8484/up`, { cache: "no-store" });
      if (!res.ok) throw new Error(`Tailnet path check failed: HTTP ${res.status}`);
      tailscaleRedirected.current = true;
      window.location.href = withSessionToken(`http://${ip}:8484/`);
    } catch (err) {
      toast.error(tailnetErrorMessage(err));
    } finally {
      setTailscaleProving(false);
    }
  }
  // Close-LAN gate: same browser tailnet-path test as the Tailscale box, but
  // without the redirect — close stays disabled until this passes.
  const [lanPathOk, setLanPathOk] = useState(false);
  const [lanPathChecking, setLanPathChecking] = useState(false);

  async function checkLanTailnetPath(ip: string) {
    setLanPathChecking(true);
    try {
      const res = await fetch(`http://${ip}:8484/up`, { cache: "no-store" });
      if (!res.ok) throw new Error(`Tailnet path check failed: HTTP ${res.status}`);
      setLanPathOk(true);
      toast.success("Tailnet path check passed — close is enabled");
    } catch (err) {
      setLanPathOk(false);
      toast.error(tailnetErrorMessage(err));
    } finally {
      setLanPathChecking(false);
    }
  }

  // A new tailnet IP invalidates the earlier check.
  useEffect(() => {
    setLanPathOk(false);
  }, [tailscaleQuery.data?.ip]);

  useEffect(() => {
    const st = tailscaleQuery.data;
    if (tailscaleLoginActive && st?.running && st.ip && !tailscaleRedirected.current && !tailscaleAutoTried.current) {
      tailscaleAutoTried.current = true;
      void proveTailscaleAndRedirect(st.ip);
    }
  });

  // Cloudflare flow: after save+reconcile, wait for https://<domain>/up then hand off.
  const [cfPending, setCfPending] = useState<string | null>(null);
  const [cfTries, setCfTries] = useState(0);
  const cfRedirected = useRef(false);
  useEffect(() => {
    if (!cfPending || cfRedirected.current) return;
    let cancelled = false;
    const tick = async () => {
      try {
        const res = await fetch(`https://${cfPending}/up`, { cache: "no-store" });
        if (!res.ok) return;
        if (cancelled || cfRedirected.current) return;
        cfRedirected.current = true;
        window.location.href = withSessionToken(`https://${cfPending}/`);
      } catch {
        // Keep polling until the certificate and DNS settle.
      }
    };
    void tick();
    const id = window.setInterval(() => {
      setCfTries((t) => t + 1);
      void tick();
    }, 5000);
    return () => {
      cancelled = true;
      window.clearInterval(id);
    };
  }, [cfPending]);
  useEffect(() => {
    if (cfTries > 72 && cfPending) {
      setCfPending(null);
      toast.error(`https://${cfPending}/up is not reachable yet — open it manually once DNS settles`);
    }
  }, [cfTries, cfPending]);

  // Recovery redo flow: ack checkbox -> copy short check code (overwrites the
  // phrase in the clipboard) -> paste it back for a local verify -> confirm kit.
  const [recovery, setRecovery] = useState<{ phrase: string[]; fingerprint: string } | null>(null);
  const [recoveryAck, setRecoveryAck] = useState(false);
  const [codeCopied, setCodeCopied] = useState(false);
  const [pasteInput, setPasteInput] = useState("");
  const [confirmedFp, setConfirmedFp] = useState<string | null>(null);
  const pasteOk =
    recovery !== null && codeCopied && pasteInput.trim().toLowerCase() === recovery.fingerprint.toLowerCase();

  const [repoKind, setRepoKind] = useState<"hetzner_storagebox" | "generic">("hetzner_storagebox");
  const [repoLabel, setRepoLabel] = useState("primary");
  const [repoUsername, setRepoUsername] = useState("");
  const [repoHost, setRepoHost] = useState("");
  const [repoSubPass, setRepoSubPass] = useState("");
  const [repoLocation, setRepoLocation] = useState("");
  const [repoPassword, setRepoPassword] = useState("");

  const reconcileMutation = useMutation({
    mutationFn: client.reconcileSetup,
    onSuccess: () => {
      toast.success("Automatic setup started");
      advance();
      void queryClient.invalidateQueries({ queryKey: ["setup"] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Reconciliation failed"),
  });

  const domainMutation = useMutation({
    mutationFn: async () => {
      if (!domain.trim()) throw new Error("Domain is required");
      await client.updateInstance({ domain: domain.trim() });
    },
    onSuccess: () => {
      toast.success("Domain saved");
      advance();
      void queryClient.invalidateQueries({ queryKey: ["instance"] });
      void queryClient.invalidateQueries({ queryKey: ["setup"] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Save failed"),
  });

  const cloudflareMutation = useMutation({
    mutationFn: async () => {
      const savedDomain = domain.trim();
      if (savedDomain) {
        await client.updateInstance({ domain: savedDomain });
      }
      const secrets: Array<{ name: string; value: string }> = [];
      if (dnsToken.trim()) secrets.push({ name: "cloudflare_dns", value: dnsToken.trim() });
      if (tunnelToken.trim()) secrets.push({ name: "cloudflare_tunnel", value: tunnelToken.trim() });
      if (zoneId.trim()) secrets.push({ name: "cloudflare_zone_id", value: zoneId.trim() });
      if (accountId.trim()) secrets.push({ name: "cloudflare_account_id", value: accountId.trim() });
      for (const s of secrets) {
        await client.createSecret({ scope: "platform-app", name: s.name, value: s.value });
      }
      await client.reconcileSetup();
      return savedDomain;
    },
    onSuccess: (savedDomain) => {
      toast.success("Cloudflare configuration saved, reconciliation started");
      advance();
      void queryClient.invalidateQueries({ queryKey: ["setup"] });
      void queryClient.invalidateQueries({ queryKey: ["instance"] });
      if (savedDomain) {
        cfRedirected.current = false;
        setCfTries(0);
        setCfPending(savedDomain);
      }
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Save failed"),
  });

  const tailscaleLoginMutation = useMutation({
    mutationFn: postTailscaleUp,
    onSuccess: (data) => {
      setTailscaleLoginActive(true);
      tailscaleAutoTried.current = false;
      tailscaleRedirected.current = false;
      if (!data.auth_url) {
        toast.success("Already enrolled — checking tailnet path");
      } else {
        window.open(data.auth_url, "_blank", "noopener,noreferrer");
        toast.success("Login opened — complete it in the new tab");
      }
      void tailscaleQuery.refetch();
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Login failed"),
  });
  const closeLanMutation = useMutation({
    mutationFn: postCloseLan,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["network-status"] });
      // Hand off to the tailnet origin with the session attached: after this
      // returns, the LAN origin stops answering, so staying would strand the
      // window on a dead page (and a manual move loses sessionStorage, which
      // is why it asked for a token). Skip when already on tailnet/HTTPS.
      const ip = tailscaleQuery.data?.ip ?? "";
      const host = window.location.hostname;
      if (ip !== "" && host !== ip && window.location.protocol !== "https:") {
        window.location.href = withSessionToken(`http://${ip}:8484/`);
        return;
      }
      toast.success("LAN dashboard closed — this window should already be on the tailnet address or HTTPS");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Close failed"),
  });

  const openLanMutation = useMutation({
    mutationFn: postOpenLan,
    onSuccess: () => {
      toast.success("LAN dashboard reopened");
      void queryClient.invalidateQueries({ queryKey: ["network-status"] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Reopen failed"),
  });

  const sshAddMutation = useMutation({
    mutationFn: async () => {
      const github = sshGithubUser.trim();
      const keys = sshPaste.trim() ? sshPaste.trim().split("\n").map((k) => k.trim()).filter(Boolean) : [];
      if (!github && keys.length === 0) throw new Error("Enter a GitHub username or paste at least one public key");
      return client.addSystemSSHKeys({ github_user: github || undefined, keys: keys.length ? keys : undefined });
    },
    onSuccess: (data) => {
      toast.success(data.added === 0 ? "No new keys added (already present)" : `Added ${data.added} key(s)`);
      setSshGithubUser("");
      setSshPaste("");
      advance();
      void queryClient.invalidateQueries({ queryKey: ["system-ssh-keys"] });
      void queryClient.invalidateQueries({ queryKey: ["setup"] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Add keys failed"),
  });

  function submitSSH(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    void sshAddMutation.mutate();
  }

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
      advance();
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
      return data as { phrase: string[]; fingerprint: string };
    },
    onSuccess: (data) => {
      setRecovery(data);
      setRecoveryAck(false);
      setCodeCopied(false);
      setPasteInput("");
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Generation failed"),
  });

  const recoveryConfirmMutation = useMutation({
    mutationFn: async () => {
      if (!recovery) throw new Error("No phrase");
      const res = await fetch("/api/v1/recovery/confirm", {
        method: "POST",
        headers: { "Content-Type": "application/json", ...authHeaders() },
        body: JSON.stringify({ fingerprint: recovery.fingerprint, saved_ack: true }),
      });
      if (!res.ok) {
        const data = (await res.json().catch(() => ({}))) as { error?: { message?: string } };
        throw new Error(data?.error?.message ?? `HTTP ${res.status}`);
      }
    },
    onSuccess: () => {
      if (recovery) setConfirmedFp(recovery.fingerprint);
      setRecovery(null);
      setRecoveryAck(false);
      setCodeCopied(false);
      setPasteInput("");
      toast.success("Recovery kit saved to the server");
      advance();
      void queryClient.invalidateQueries({ queryKey: ["setup"] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Confirm failed"),
  });

  const copyCheckCode = async () => {
    if (!recovery) return;
    const ok = await copyText(recovery.fingerprint);
    if (ok) {
      setCodeCopied(true);
      toast.success("Check code copied — paste it below");
    } else {
      toast.error("Copy failed — type the fingerprint manually");
    }
  };

  const repoMutation = useMutation({
    mutationFn: async () => {
      const payload: Record<string, string> = { label: repoLabel.trim() };
      if (repoKind === "hetzner_storagebox") {
        payload.kind = "hetzner_storagebox";
        payload.username = repoUsername.trim();
        payload.host = repoHost.trim();
        payload.sub_account_password = repoSubPass;
      } else {
        payload.location = repoLocation.trim();
        payload.password = repoPassword;
      }
      const res = await fetch("/api/v1/backup-repositories", {
        method: "POST",
        headers: { "Content-Type": "application/json", ...authHeaders() },
        body: JSON.stringify(payload),
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data?.error?.message ?? `HTTP ${res.status}`);
      return data;
    },
    onSuccess: () => {
      toast.success("Backup repository configured; daily backup timer enabled. First backup and verify running in background.");
      setRepoSubPass("");
      setRepoPassword("");
      advance();
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
      advance();
      void queryClient.invalidateQueries({ queryKey: ["setup"] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "Woodpecker connect failed"),
  });

  if (setupQuery.isLoading) return <LoadingState label="Loading setup status" />;
  if (setupQuery.isError) return <ErrorState error={setupQuery.error} retry={() => void setupQuery.refetch()} />;
  const setup = setupQuery.data;
  if (!setup) return <LoadingState label="Loading setup status" />;

  const checkById = (id: string) => setup.checks.find((c) => c.id === id);
  const isOk = (id: string) => checkById(id)?.status === "ok";

  const hasUsers = (usersQuery.data?.length ?? 0) > 0;
  const instanceDomain = instanceQuery.data?.domain ?? "";
  const domainValid = !!instanceDomain && instanceDomain !== "example.com" && instanceDomain !== "not-configured.invalid";
  const sshHost = instanceDomain || (typeof window !== "undefined" ? window.location.hostname : "your-server");
  const recoveryEmail = usersQuery.data?.[0]?.email ?? inviteEmail ?? "admin@example.com";

  const adminCheck = checkById("admin_passkeys");
  const passkeyCount = adminCheck?.passkey_count ?? 0;
  const passkeyTarget = adminCheck?.target ?? 2;
  const coreAppsCheck = checkById("core_apps");
  // Login stage: admin creation needs Caddy + Pocket ID only. Remaining
  // apps finish in their own per-app steps later in the flow.
  const coreAppList = coreAppsCheck?.apps ?? [];
  const isIdentityReady =
    coreAppList.some((a) => a.bundle_id === "caddy" && a.status === "running") &&
    coreAppList.some((a) => a.bundle_id === "pocket-id" && a.status === "running");
  const isIdentityNotConfigured = adminCheck?.detail?.includes("identity not configured");
  const inviteDisabled = inviteMutation.isPending || !isIdentityReady || !!isIdentityNotConfigured;
  const woodpeckerCheck = checkById("woodpecker_connection");

  const tailscaleRunning = tailscaleQuery.data?.running === true;
  const tailscaleIp = tailscaleQuery.data?.ip ?? "";
  const lanClosed = networkQuery.data?.lan_closed === true;
  const doneMap: Record<BoxId, boolean> = {
    ssh: isOk("ssh_keys"),
    tailscale: tailscaleRunning || isOk("tailscale"),
    domain: domainValid && isOk("domain"),
    cloudflare: isOk("cloudflare_dns"),
    lan: lanClosed,
    admin: isOk("admin_passkeys"),
    recovery: isOk("recovery_key"),
    backups: isOk("backups_configured"),
    storage: isOk("storage_configured"),
    woodpecker: woodpeckerCheck?.status === "ok",
  };
  const autoOpen: BoxId | null = BLOCKING.find((id) => !doneMap[id]) ?? null;
  const openId = openOverride !== undefined ? openOverride : autoOpen;
  const toggle = (id: BoxId) => setOpenOverride(openId === id ? null : id);
  const identityAppIds = ["caddy", "pocket-id"];
  const identityApps = coreAppList.filter((a) => identityAppIds.includes(a.bundle_id));
  const otherApps = coreAppList.filter((a) => !identityAppIds.includes(a.bundle_id));


  return (
    <div className="page">
      <style>{`
        .setup-accordion { display: grid; gap: 10px; margin-top: 12px; }
        .setup-divider { border: none; border-top: 1px solid var(--line); margin: 12px 0 0; }
        .setup-box { background: #111418; color: #d7dce2; border: 1px solid #2a3138; border-radius: 8px; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 0.85rem; }
        .setup-box input, .setup-box textarea, .setup-box button { font-family: inherit; }
        .setup-box-head { display: flex; align-items: center; justify-content: flex-start; gap: 8px; width: 100%; padding: 10px 12px; background: none; border: none; color: inherit; font-size: inherit; cursor: pointer; text-align: left; }
        .setup-box-title { font-weight: 600; }
        .setup-tag[data-tone="done"] { color: #4ade80; }
        .setup-tag[data-tone="todo"] { color: #f87171; }
        .setup-box-body { padding: 0 12px 12px; display: grid; gap: 8px; }
      `}</style>
      <header className="page-header">
        <div>
          <h1>Setup &amp; Connect Services</h1>
        </div>
        <div className="page-actions">
          <button className="button secondary" type="button" onClick={() => void reconcileMutation.mutate()} disabled={reconcileMutation.isPending}>
            {reconcileMutation.isPending ? "Retrying…" : "Retry automatic setup"}
          </button>
        </div>
      </header>
      <hr className="setup-divider" />
      <div className="setup-accordion">
        <Box title="SSH keys" done={doneMap.ssh} open={openId === "ssh"} onToggle={() => toggle("ssh")}>
          <p>The installer seeded keys for the administrator. Add more here if needed.</p>
          {sshQuery.isLoading ? (
            <p>Loading installed keys…</p>
          ) : sshQuery.isError ? (
            <div style={{ display: "flex", gap: 8, flexWrap: "wrap", alignItems: "center" }}>
              <span className="inline-error" role="alert">{sshQuery.error instanceof Error ? sshQuery.error.message : "Failed to load keys"}</span>
              <button className="button ghost" type="button" onClick={() => void sshQuery.refetch()}>Retry</button>
            </div>
          ) : (
            <>
              {sshQuery.data && (
                <p>Administrator username: <strong className="mono">{sshQuery.data.username}</strong> <CopyButton text={sshQuery.data.username} label="Copy username" /></p>
              )}
              {!sshQuery.data?.items.length ? (
                <p>No SSH keys installed yet — remote SSH stays unavailable until you add one.</p>
              ) : (
                <ul style={{ listStyle: "none", padding: 0, display: "grid", gap: 6 }}>
                  {sshQuery.data.items.map((k) => (
                    <li key={k.fingerprint} style={{ display: "flex", gap: 8, alignItems: "center", fontSize: "0.9em", wordBreak: "break-all", overflowWrap: "anywhere" }}>
                      <span className="mono">{k.type}</span>
                      <span className="mono">{k.fingerprint}</span>
                      <span style={{ opacity: 0.7 }}>{k.comment || "no comment"}</span>
                      <CopyButton text={k.fingerprint} label="Copy fingerprint" />
                    </li>
                  ))}
                </ul>
              )}
            </>
          )}
          <form className="form-stack" onSubmit={submitSSH}>
            <label className="field">
              <span>GitHub username</span>
              <input value={sshGithubUser} onChange={(e) => setSshGithubUser(e.target.value)} placeholder="your-github-username" autoComplete="off" />
            </label>
            <label className="field">
              <span>…or paste public keys (one per line)</span>
              <textarea value={sshPaste} onChange={(e) => setSshPaste(e.target.value)} rows={4} placeholder="ssh-ed25519 AAAA…" className="mono" />
            </label>
            <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
              <button className="button primary" type="submit" disabled={sshAddMutation.isPending}>
                {sshAddMutation.isPending ? "Adding…" : "Add keys"}
              </button>
              <button className="button ghost" type="button" onClick={() => { void sshQuery.refetch(); void setupQuery.refetch(); }}>
                Refresh status
              </button>
            </div>
            {sshAddMutation.isError && (
              <p className="inline-error" role="alert">{sshAddMutation.error instanceof Error ? sshAddMutation.error.message : "Add keys failed"}</p>
            )}
          </form>
        </Box>
        <Box title="Tailscale" done={doneMap.tailscale} open={openId === "tailscale"} onToggle={() => toggle("tailscale")}>
          <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
            <button className="button primary" type="button" onClick={() => void tailscaleLoginMutation.mutate()} disabled={tailscaleLoginMutation.isPending || tailscaleProving}>
              {tailscaleLoginMutation.isPending ? "Starting…" : tailscaleRunning ? "Re-check tailnet path" : "Start Tailscale login"}
            </button>
            {tailscaleRunning && tailscaleIp && (
              <button className="button secondary" type="button" onClick={() => void proveTailscaleAndRedirect(tailscaleIp)} disabled={tailscaleProving}>
                {tailscaleProving ? "Checking…" : "Open over Tailscale"}
              </button>
            )}
            <button className="button ghost" type="button" onClick={() => void tailscaleQuery.refetch()}>
              Refresh status
            </button>
          </div>
          {tailscaleLoginActive && !tailscaleRunning && <p>Login started — complete it in the new tab, status polls every 5s.</p>}
          {tailscaleQuery.isError && (
            <p className="inline-error" role="alert">{tailscaleQuery.error instanceof Error ? tailscaleQuery.error.message : "Status check failed"}</p>
          )}
          {tailscaleLoginMutation.isError && (
            <p className="inline-error" role="alert">{tailscaleLoginMutation.error instanceof Error ? tailscaleLoginMutation.error.message : "Login failed"}</p>
          )}
        </Box>

        <Box title="Domain" done={doneMap.domain} open={openId === "domain"} onToggle={() => toggle("domain")}>
          <label className="field">
            <span>Domain</span>
            <input value={domain} onChange={(e) => setDomain(e.target.value)} placeholder="example.com" />
            {domainValid && <small className="muted">Current: {instanceDomain} — edit to change</small>}
          </label>
          <div>
            <button className="button primary" type="button" onClick={() => void domainMutation.mutate()} disabled={domainMutation.isPending}>
              {domainMutation.isPending ? "Saving…" : "Save domain"}
            </button>
          </div>
          {domainMutation.isError && (
            <p className="inline-error" role="alert">{domainMutation.error instanceof Error ? domainMutation.error.message : "Save failed"}</p>
          )}
        </Box>

        <Box title="Cloudflare DNS" done={doneMap.cloudflare} open={openId === "cloudflare"} onToggle={() => toggle("cloudflare")}>
          <div className="form-stack">
            <label className="field">
              <span>DNS token</span>
              <small className="muted">Cloudflare API token with Zone:Read + DNS:Edit</small>
              <input type="password" value={dnsToken} onChange={(e) => setDnsToken(e.target.value)} placeholder="dns token" />
              {existingSecrets["cloudflare_dns"] && <small className="muted">Already set — leave blank to keep, or enter new value to overwrite.</small>}
            </label>
            <label className="field">
              <span>Tunnel token (optional)</span>
              <input type="password" value={tunnelToken} onChange={(e) => setTunnelToken(e.target.value)} placeholder="tunnel token (optional)" />
              {existingSecrets["cloudflare_tunnel"] && <small className="muted">Already set — leave blank to keep, or enter new value to overwrite.</small>}
            </label>
            <label className="field">
              <span>Zone ID (optional)</span>
              <input value={zoneId} onChange={(e) => setZoneId(e.target.value)} placeholder="zone id" />
              {existingSecrets["cloudflare_zone_id"] && <small className="muted">Already set — leave blank to keep, or enter new value to overwrite.</small>}
            </label>
            <label className="field">
              <span>Cloudflare Account ID (32-hex, sidebar, optional)</span>
              <small className="muted">From Cloudflare dashboard sidebar — 32 hex characters (a-f, 0-9), not an email. Open dash.cloudflare.com → sidebar shows Account ID.</small>
              <input value={accountId} onChange={(e) => setAccountId(e.target.value)} placeholder="e.g. 9b1a2c3d4e5f6a7b8c9d0e1f2a3b4c5d" />
              {existingSecrets["cloudflare_account_id"] && <small className="muted">Already set — leave blank to keep, or enter new value to overwrite (32-hex, re-validated on save).</small>}
            </label>
            <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
              <button className="button secondary" type="button" onClick={() => void verifyTokenMutation.mutate()} disabled={verifyTokenMutation.isPending || !dnsToken.trim()}>
                {verifyTokenMutation.isPending ? "Verifying…" : "Verify DNS token"}
              </button>
              <button className="button primary" type="button" onClick={() => void cloudflareMutation.mutate()} disabled={cloudflareMutation.isPending}>
                {cloudflareMutation.isPending ? "Saving…" : "Save and reconcile"}
              </button>
            </div>
            {cfPending && <p>Saved — waiting for https://{cfPending}/up, then redirecting to the public address…</p>}
            {cloudflareMutation.isError && (
              <p className="inline-error" role="alert">{cloudflareMutation.error instanceof Error ? cloudflareMutation.error.message : "Save failed"}</p>
            )}
            {domainValid && (
              <p>
                Identity: <a href={`https://id.${instanceDomain}`} target="_blank" rel="noreferrer">{`https://id.${instanceDomain}`}</a>
              </p>
            )}
          </div>
        </Box>
        <Box title="Close LAN access" done={doneMap.lan} open={openId === "lan"} onToggle={() => toggle("lan")}>
          {lanClosed ? (
            <>
              <p>LAN dashboard is cut. This window should already be on the tailnet address or HTTPS — LAN :8484 will no longer answer.</p>
              <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
                <button className="button primary" type="button" onClick={() => void openLanMutation.mutate()} disabled={openLanMutation.isPending}>
                  {openLanMutation.isPending ? "Reopening…" : "Reopen LAN access"}
                </button>
                <button className="button ghost" type="button" onClick={() => void networkQuery.refetch()}>
                  Refresh status
                </button>
              </div>
            </>
          ) : (
            <>
              {!tailscaleRunning || !tailscaleIp ? (
                <p>Enroll Tailscale first — close requires a working tailnet path.</p>
              ) : (
                <>
                  <p>
                    Prove the tailnet path from this browser first — the same check as the Tailscale box
                    (fetch http://{tailscaleIp}:8484/up). Close stays disabled until it passes.
                  </p>
                  <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
                    <button className="button secondary" type="button" onClick={() => void checkLanTailnetPath(tailscaleIp)} disabled={lanPathChecking}>
                      {lanPathChecking ? "Checking…" : lanPathOk ? "Re-check tailnet path" : "Check tailnet path"}
                    </button>
                    <button
                      className="button primary"
                      type="button"
                      onClick={() => void closeLanMutation.mutate()}
                      disabled={!lanPathOk || lanPathChecking || closeLanMutation.isPending}
                    >
                      {closeLanMutation.isPending ? "Closing…" : "Close LAN access"}
                    </button>
                    <button className="button ghost" type="button" onClick={() => void networkQuery.refetch()}>
                      Refresh status
                    </button>
                  </div>
                  {lanPathOk && <p>Tailnet path OK — closing now is safe from this browser.</p>}
                </>
              )}
            </>
          )}
          {networkQuery.isError && (
            <p className="inline-error" role="alert">{networkQuery.error instanceof Error ? networkQuery.error.message : "Status check failed"}</p>
          )}
          {closeLanMutation.isError && (
            <p className="inline-error" role="alert">{closeLanMutation.error instanceof Error ? closeLanMutation.error.message : "Close failed"}</p>
          )}
          {openLanMutation.isError && (
            <p className="inline-error" role="alert">{openLanMutation.error instanceof Error ? openLanMutation.error.message : "Reopen failed"}</p>
          )}
        </Box>

        {identityApps.map((a) => (
          <Box key={`core-${a.bundle_id}`} title={`Login: ${a.bundle_id}`} done={a.status === "running"} open={coreOpen === a.bundle_id} onToggle={() => setCoreOpen(coreOpen === a.bundle_id ? null : a.bundle_id)}>
            <p>Status: {a.status}{a.detail ? ` — ${a.detail}` : ""}</p>
            <p>Required before admin creation — login cannot work until Caddy + Pocket ID are healthy.</p>
            <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
              <button className="button secondary" type="button" onClick={() => { void setupQuery.refetch(); }} disabled={setupQuery.isFetching}>
                {setupQuery.isFetching ? "Checking…" : "Refresh status"}
              </button>
              <button className="button primary" type="button" onClick={() => void reconcileMutation.mutate()} disabled={reconcileMutation.isPending}>
                {reconcileMutation.isPending ? "Retrying…" : "Retry automatic setup"}
              </button>
            </div>
          </Box>
        ))}
        <Box title="Admin account + passkeys" done={doneMap.admin} open={openId === "admin"} onToggle={() => toggle("admin")}>
          {!hasUsers ? (
            <div className="form-stack">
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
              {(!isIdentityReady || isIdentityNotConfigured) && (
                <p className="inline-error" role="alert">Waiting on Caddy + Pocket ID (login stage) — other apps finish in their own steps below.</p>
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
            <div style={{ marginTop: 12, padding: 12, border: "var(--border) solid var(--line)", borderRadius: 6 }}>
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
        </Box>
        {otherApps.map((a) => (
          <Box key={`core-${a.bundle_id}`} title={a.bundle_id} done={a.status === "running"} open={coreOpen === a.bundle_id} onToggle={() => setCoreOpen(coreOpen === a.bundle_id ? null : a.bundle_id)}>
            <p>Status: {a.status}{a.detail ? ` — ${a.detail}` : ""}</p>
            <p>Independent step — enable whenever ready. Some apps need extra information to work correctly.</p>
            <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
              <button className="button secondary" type="button" onClick={() => { void setupQuery.refetch(); }} disabled={setupQuery.isFetching}>
                {setupQuery.isFetching ? "Checking…" : "Refresh status"}
              </button>
              <button className="button primary" type="button" onClick={() => void reconcileMutation.mutate()} disabled={reconcileMutation.isPending}>
                {reconcileMutation.isPending ? "Retrying…" : "Retry automatic setup"}
              </button>
            </div>
          </Box>
        ))}

        <Box title="Recovery phrase" done={doneMap.recovery} open={openId === "recovery"} onToggle={() => toggle("recovery")}>
          {!recovery ? (
            <div className="form-stack">
              {confirmedFp && <p>Kit stored on the server (fingerprint {confirmedFp}). Generating again replaces it.</p>}
              <button className="button primary" type="button" onClick={() => void recoveryGenerateMutation.mutate()} disabled={recoveryGenerateMutation.isPending}>
                {recoveryGenerateMutation.isPending ? "Generating…" : confirmedFp ? "Generate a new phrase" : "Generate recovery phrase"}
              </button>
              {recoveryGenerateMutation.isError && (
                <p className="inline-error" role="alert">{recoveryGenerateMutation.error instanceof Error ? recoveryGenerateMutation.error.message : "Generation failed"}</p>
              )}
            </div>
          ) : (
            <div className="form-stack">
              <div style={{ display: "grid", gridTemplateColumns: "repeat(4, 1fr)", gap: 8, background: "var(--surface-raised)", padding: 12, borderRadius: 6 }}>
                {recovery.phrase.map((w, i) => (
                  <div key={i} style={{ display: "flex", gap: 6, alignItems: "center", fontFamily: "monospace", fontSize: "0.9em" }}>
                    <span style={{ opacity: 0.6, minWidth: 20 }}>{i + 1}.</span>
                    <span>{w}</span>
                  </div>
                ))}
              </div>
              <div style={{ display: "flex", gap: 8, alignItems: "center", flexWrap: "wrap" }}>
                <CopyButton text={recovery.phrase.join(" ")} label="Copy phrase" />
                <span style={{ opacity: 0.7, fontSize: "0.85em" }}>Fingerprint: {recovery.fingerprint}</span>
              </div>
              <p style={{ opacity: 0.9 }}>This phrase unlocks your backups and this server if everything else is lost. Store it offline (Bitwarden, 1Password, paper). Omahab cannot show it again.</p>
              <label style={{ display: "flex", gap: 8, alignItems: "center" }}>
                <input type="checkbox" checked={recoveryAck} onChange={(e) => setRecoveryAck(e.target.checked)} />
                I saved this in my password manager
              </label>
              {recoveryAck && !codeCopied && (
                <div>
                  <button className="button secondary" type="button" onClick={() => void copyCheckCode()}>
                    Copy check code
                  </button>
                  <p style={{ opacity: 0.8, fontSize: "0.9em" }}>Copies a short check code (this overwrites the phrase in your clipboard), then paste it back below to verify.</p>
                </div>
              )}
              {codeCopied && (
                <>
                  <label className="field">
                    <span>Paste the check code back</span>
                    <input value={pasteInput} onChange={(e) => setPasteInput(e.target.value)} placeholder={recovery.fingerprint} autoComplete="off" />
                  </label>
                  {pasteInput.trim() !== "" && !pasteOk && (
                    <p className="inline-error" role="alert">Does not match — paste the code you just copied.</p>
                  )}
                  <div style={{ display: "flex", gap: 8 }}>
                    <button className="button secondary" type="button" onClick={() => { setCodeCopied(false); setPasteInput(""); }}>
                      Back
                    </button>
                    <button
                      className="button primary"
                      type="button"
                      onClick={() => void recoveryConfirmMutation.mutate()}
                      disabled={recoveryConfirmMutation.isPending || !pasteOk}
                    >
                      {recoveryConfirmMutation.isPending ? "Confirming…" : "Confirm and store kit"}
                    </button>
                  </div>
                </>
              )}
              {recoveryConfirmMutation.isError && (
                <p className="inline-error" role="alert">{recoveryConfirmMutation.error instanceof Error ? recoveryConfirmMutation.error.message : "Confirm failed"}</p>
              )}
            </div>
          )}
        </Box>

        <Box title="Backups" done={doneMap.backups} open={openId === "backups"} onToggle={() => toggle("backups")}>
          <div className="form-stack">
            <div style={{ display: "flex", gap: 8, marginBottom: 8, flexWrap: "wrap" }}>
              <button type="button" className={`button ${repoKind === "hetzner_storagebox" ? "primary" : "ghost"}`} onClick={() => setRepoKind("hetzner_storagebox")}>Hetzner Storage Box</button>
              <button type="button" className={`button ${repoKind === "generic" ? "primary" : "ghost"}`} onClick={() => setRepoKind("generic")}>Advanced (restic URL)</button>
            </div>
            <label className="field">
              <span>Label</span>
              <input value={repoLabel} onChange={(e) => setRepoLabel(e.target.value)} placeholder="primary" />
            </label>
            {repoKind === "hetzner_storagebox" ? (
              <>
                <label className="field">
                  <span>Username (u123456)</span>
                  <input value={repoUsername} onChange={(e) => setRepoUsername(e.target.value)} placeholder="u123456" autoComplete="off" />
                </label>
                <label className="field">
                  <span>Host (u123456.your-storagebox.de)</span>
                  <input value={repoHost} onChange={(e) => setRepoHost(e.target.value)} placeholder="u123456.your-storagebox.de" autoComplete="off" />
                </label>
                <label className="field">
                  <span>Sub-account password (used once to upload SSH key)</span>
                  <input type="password" value={repoSubPass} onChange={(e) => setRepoSubPass(e.target.value)} autoComplete="new-password" />
                </label>
              </>
            ) : (
              <>
                <label className="field">
                  <span>Location (restic URL)</span>
                  <input value={repoLocation} onChange={(e) => setRepoLocation(e.target.value)} placeholder="sftp:user@host:restic-repo" className="mono" />
                </label>
                <label className="field">
                  <span>Repository password</span>
                  <input type="password" value={repoPassword} onChange={(e) => setRepoPassword(e.target.value)} autoComplete="new-password" />
                </label>
              </>
            )}
            <button
              className="button primary"
              type="button"
              onClick={() => void repoMutation.mutate()}
              disabled={
                repoMutation.isPending ||
                !repoLabel.trim() ||
                (repoKind === "hetzner_storagebox"
                  ? !repoUsername.trim() || !repoHost.trim() || !repoSubPass
                  : !repoLocation.trim() || !repoPassword)
              }
            >
              {repoMutation.isPending ? "Configuring…" : "Add repository"}
            </button>
            {repoMutation.isError && (
              <p className="inline-error" role="alert">{repoMutation.error instanceof Error ? repoMutation.error.message : "Configure failed"}</p>
            )}
            <p>Run on the server as root to test recovery for {recoveryEmail}:</p>
            <div style={{ display: "flex", gap: 8, alignItems: "center", background: "var(--surface-raised)", padding: 8, borderRadius: 6 }}>
              <code style={{ flex: 1, wordBreak: "break-all" }}>ssh {sshHost} sudo omahab identity recover {recoveryEmail}</code>
              <CopyButton text={`ssh ${sshHost} sudo omahab identity recover ${recoveryEmail}`} label="Copy" />
            </div>
          </div>
        </Box>

        <Box title="Storage (optional)" done={doneMap.storage} open={openId === "storage"} onToggle={() => toggle("storage")}>
          <div style={{ display: "flex", gap: 8, alignItems: "center", background: "var(--surface-raised)", padding: 8, borderRadius: 6 }}>
            <code style={{ flex: 1, wordBreak: "break-all" }}>curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8484/api/v1/system/disks</code>
            <CopyButton text='curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8484/api/v1/system/disks' label="Copy" />
          </div>
        </Box>

        <Box title="Woodpecker (manual)" done={doneMap.woodpecker} open={openId === "woodpecker"} onToggle={() => toggle("woodpecker")}>
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
                Status: {woodpeckerCheck.status}
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
              <p style={{ color: "var(--positive)", fontSize: "0.9em" }}>Woodpecker connected. Token cleared and not displayed.</p>
            )}
          </div>
        </Box>
      </div>
    </div>
  );
}
