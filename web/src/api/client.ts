import type {
  AddSSHKeysRequest,
  AddSSHKeysResponse,
  TechnitiumSetup,
  ApiErrorEnvelope,
  Application,
  Backup,
  CatalogBundle,
  CompanionDevice,
  ControlEvent,
  CreateCompanionEnrollmentResponse,
  CreateModelKeyResponse,
  DeleteSSHKeyRequest,
  Disk,
  DoctorReport,
  Exposure,
  ExposureState,
  IndexSetupOption,
  Instance,
  KnowledgeConsent,
  ListEnvelope,
  Maintenance,
  ModelAliasName,
  ModelInfo,
  ModelKey,
  ModelSetupStatus,
  NtfyConfig,
  OAuthSession,
  Operation,
  Project,
  PublicStatusResponse,
  RecoverySession,
  Release,
  Secret,
  SetupStatus,
  SSHKey,
  Status,
  StoragePlacement,
  SyncFolder,
  SystemSSHKeysResponse,
  ToolVariableMeta,
  User,
  Workspace,
} from "./types";
// API base is relative: fetch("/api/v1/...") resolves to the same host the user
// loaded the WebUI from (e.g. http://192.168.1.12:8484 or http://omahab.local:8484).
// This ensures first-boot surfaces render the device LAN IP / <hostname>.local
// and never 127.0.0.1. See console.go lanIPv4() / bootstrapClaimURL() pattern.
const API_ROOT = "/api/v1";

export class ApiError extends Error {
  constructor(
    message: string,
    readonly code: string,
    readonly status: number,
  ) {
    super(message);
    this.name = "ApiError";
  }
}


function isEnvelope<T>(value: T[] | ListEnvelope<T>): value is ListEnvelope<T> {
  return !Array.isArray(value);
}

export class EventConsumerError extends Error {
  constructor(message = "The live event consumer failed.", options?: ErrorOptions) {
    super(message, options);
    this.name = "EventConsumerError";
  }
}

function parseControlEvent(data: string): ControlEvent | null {
  let raw: unknown;
  try {
    raw = JSON.parse(data);
  } catch {
    return null;
  }
  if (typeof raw !== "object" || raw === null) return null;
  const v = raw as Record<string, unknown>;
  if (
    typeof v.id !== "string" ||
    typeof v.type !== "string" ||
    typeof v.severity !== "string" ||
    typeof v.message !== "string" ||
    typeof v.created_at !== "string"
  ) {
    return null;
  }
  if (v.resource_id !== undefined && typeof v.resource_id !== "string") return null;
  if (v.read_at !== undefined && typeof v.read_at !== "string") return null;
  if (v.data !== undefined && (typeof v.data !== "object" || v.data === null)) return null;
  return raw as ControlEvent;
}

export class ApiClient {
  constructor(private readonly getToken: () => string | null) {}

  private async request<T>(path: string, init: RequestInit = {}): Promise<T> {
    const token = this.getToken();
    const headers = new Headers(init.headers);
    headers.set("Accept", "application/json");
    if (token) headers.set("Authorization", `Bearer ${token}`);
    if (init.body) headers.set("Content-Type", "application/json");

    let response: Response;
    try {
      response = await fetch(`${API_ROOT}${path}`, { ...init, headers });
    } catch (error) {
      throw new ApiError(error instanceof Error ? error.message : "The server could not be reached.", "network_error", 0);
    }

    if (!response.ok) {
      let detail: ApiErrorEnvelope | undefined;
      try {
        detail = (await response.json()) as ApiErrorEnvelope;
      } catch {
        // The status text remains a useful fallback for non-JSON proxy errors.
      }
      if (response.status === 401) window.dispatchEvent(new Event("omahab:unauthorized"));
      throw new ApiError(detail?.error.message ?? response.statusText ?? "Request failed", detail?.error.code ?? "request_failed", response.status);
    }

    if (response.status === 204) return undefined as T;
    return (await response.json()) as T;
  }

  private async list<T>(path: string): Promise<T[]> {
    const value = await this.request<T[] | ListEnvelope<T>>(path);
    return isEnvelope(value) ? value.items : value;
  }

  status = () => this.request<Status>("/status");
  catalog = () => this.list<CatalogBundle>("/catalog");
  applications = () => this.list<Application>("/applications");
  installApplication = (input: { bundle_id: string; name?: string; hostname?: string; exposure?: CatalogBundle["default_exposure"] }) =>
    this.request<Application>("/applications", { method: "POST", body: JSON.stringify(input) });
  applicationAction = (id: string, action: "start" | "stop" | "restart" | "update" | "rollback") =>
    this.request<Application>(`/applications/${encodeURIComponent(id)}/actions`, {
      method: "POST",
      body: JSON.stringify({ action }),
    });
  pauseApplicationMaintenance = (id: string) =>
    this.request<Maintenance>(`/applications/${encodeURIComponent(id)}/maintenance`, {
      method: "POST",
      body: JSON.stringify({ reason: "import" }),
    });
  resumeApplicationMaintenance = (id: string) =>
    this.request<{ resumed: boolean }>(`/applications/${encodeURIComponent(id)}/maintenance`, {
      method: "DELETE",
      body: JSON.stringify({ confirm: true }),
    });
  setExposure = (resource: "applications" | "projects", id: string, exposure: Exposure, confirmation?: string) =>
    this.request<ExposureState>(`/${resource}/${encodeURIComponent(id)}/exposure`, {
      method: "PUT",
      body: JSON.stringify({ exposure, confirmation }),
    });

  projects = () => this.list<Project>("/projects");
  createProject = (input: { name: string; slug?: string; kind?: string }) =>
    this.request<Project>("/projects", { method: "POST", body: JSON.stringify(input) });
  releases = (projectId: string) => this.list<Release>(`/projects/${encodeURIComponent(projectId)}/releases`);
  rollbackRelease = (projectId: string, releaseId: string) =>
    this.request<Release>(`/projects/${encodeURIComponent(projectId)}/releases/${encodeURIComponent(releaseId)}/rollback`, {
      method: "POST",
      body: JSON.stringify({}),
    });

  disks = () => this.list<Disk>("/system/disks");
  configureStorage = (input: { placements: StoragePlacement[]; migrate?: boolean; confirm?: boolean }) =>
    this.request<Operation>("/system/storage", { method: "PUT", body: JSON.stringify(input) });
  operation = (id: string) => this.request<Operation>(`/operations/${encodeURIComponent(id)}`);
  operationAction = (id: string, action: "resume" | "rollback" | "dismiss") =>
    this.request<Operation>(`/operations/${encodeURIComponent(id)}/actions`, {
      method: "POST",
      body: JSON.stringify({ action, confirm: true }),
    });
  technitiumSetup = () => this.request<TechnitiumSetup>("/technitium/setup");
  backups = () => this.list<Backup>("/backups");
  createBackup = (input?: { repository?: string; scopes?: string[] }) =>
    this.request<Backup>("/backups", { method: "POST", body: JSON.stringify(input ?? {}) });
  verifyBackup = (id: string) => this.request<Backup>(`/backups/${encodeURIComponent(id)}/verify`, { method: "POST", body: JSON.stringify({}) });
  restoreBackup = (id: string, input?: { scopes?: string[]; mode?: string; confirm?: boolean }) =>
    this.request<Backup>(`/backups/${encodeURIComponent(id)}/restore`, { method: "POST", body: JSON.stringify(input ?? {}) });
  importRepoSnapshots = (repositoryId: string) =>
    this.request<{ imported: number }>(`/backup-repositories/${encodeURIComponent(repositoryId)}/snapshots/import`, { method: "POST", body: JSON.stringify({}) });

  events = () => this.list<ControlEvent>("/events");
  markEventRead = (id: string) => this.request<ControlEvent>(`/events/${encodeURIComponent(id)}/read`, { method: "POST", body: JSON.stringify({}) });
  markAllEventsRead = () => this.request<void>("/events/read-all", { method: "POST" });

  syncFolders = () => this.list<SyncFolder>("/sync/folders");
  createSyncFolder = (input: { name: string; server_path: string; share_with_ai: boolean }) =>
    this.request<SyncFolder>("/sync/folders", { method: "POST", body: JSON.stringify(input) });
  updateSyncFolder = (id: string, input: { share_with_ai: boolean }) =>
    this.request<SyncFolder>(`/sync/folders/${encodeURIComponent(id)}`, { method: "PATCH", body: JSON.stringify(input) });

  workspaces = () => this.list<Workspace>("/workspaces");
  createWorkspace = (input: { project_id: string; branch: string; agent: string }) =>
    this.request<Workspace>("/workspaces", { method: "POST", body: JSON.stringify(input) });
  stopWorkspace = (id: string) => this.request<Workspace>(`/workspaces/${encodeURIComponent(id)}/stop`, { method: "POST", body: JSON.stringify({}) });

  users = () => this.list<User>("/users");
  createUser = (input: { name: string; email: string; groups?: string[] }) => this.request<User>("/users", { method: "POST", body: JSON.stringify(input) });
  setUserDisabled = (id: string, disabled: boolean) =>
    this.request<User>(`/users/${encodeURIComponent(id)}`, { method: "PATCH", body: JSON.stringify({ disabled }) });
  setUserGroups = (id: string, groups: string[]) =>
    this.request<User>(`/users/${encodeURIComponent(id)}`, { method: "PATCH", body: JSON.stringify({ groups }) });
  beginRecovery = (id: string) =>
    this.request<RecoverySession>(`/users/${encodeURIComponent(id)}/recovery`, { method: "POST", body: JSON.stringify({}) });
  issueEnrollment = (id: string) => this.request<User>(`/users/${encodeURIComponent(id)}/enrollment`, { method: "POST", body: JSON.stringify({}) });

  instance = () => this.request<Instance>("/instance");
  updateInstance = (input: { domain: string; assistant_name?: string }) =>
    this.request<Instance>("/instance", { method: "PUT", body: JSON.stringify(input) });
  createSecret = (input: { scope: string; name: string; value: string }) =>
    this.request<Secret>("/secrets", { method: "POST", body: JSON.stringify(input) });
  listSecrets = (scope?: string) => {
    const q = scope ? `?scope=${encodeURIComponent(scope)}` : "";
    return this.list<Secret>(`/secrets${q}`);
  };

  setup = () => this.request<SetupStatus>("/setup");
  reconcileSetup = () => this.request<void>("/setup/reconcile", { method: "POST", body: JSON.stringify({}) });
  setupWoodpecker = (input: { username: string; token: string }) =>
    this.request<{ status: string }>("/setup/woodpecker", { method: "PUT", body: JSON.stringify(input) });

  // Native model setup (LiteLLM) — read-only inventory plus explicit seed; no credential intake.
  modelSetup = () => this.request<ModelSetupStatus>("/setup/models");
  seedModelAliases = (input: { model_id: string }) =>
    this.request<ModelSetupStatus>("/setup/models/seed", { method: "POST", body: JSON.stringify(input) });

  // Model keys (virtual keys) — metadata, plaintext returned once on create
  modelKeys = () => this.list<ModelKey>("/model-keys");
  createModelKey = (input: { name: string; owner_kind: "hermes" | "device" | "harness"; owner_id: string; scopes?: ModelAliasName[]; rpm?: number; tpm?: number; concurrency?: number; budget?: number; expires_at?: string }) =>
    this.request<CreateModelKeyResponse>("/model-keys", { method: "POST", body: JSON.stringify(input) });
  deleteModelKey = (id: string) =>
    this.request<void>(`/model-keys/${encodeURIComponent(id)}`, { method: "DELETE" });

  // Provider OAuth (subscription) — safe session, no secrets
  startProviderOAuth = (provider: "chatgpt" | "xai", flow: "device_code" | "loopback") =>
    this.request<OAuthSession>(`/provider-oauth/${encodeURIComponent(provider)}/start`, { method: "POST", body: JSON.stringify({ flow }) });
  pollProviderOAuth = (provider: "chatgpt" | "xai", sessionId: string) =>
    this.request<OAuthSession>(`/provider-oauth/${encodeURIComponent(provider)}/poll/${encodeURIComponent(sessionId)}`);
  forwardProviderOAuthCallback = (provider: "xai", sessionId: string, callbackPath: string) =>
    this.request<OAuthSession>(`/provider-oauth/${encodeURIComponent(provider)}/callback/${encodeURIComponent(sessionId)}`, {
      method: "POST",
      body: JSON.stringify({ callback_path: callbackPath }),
    });

  // Tool environment (admin only, metadata only, browser never calls device endpoint)
  toolEnvironment = () => this.list<ToolVariableMeta>("/tool-environment");
  putToolVariable = (name: string, value: string) =>
    this.request<ToolVariableMeta>(`/tool-environment/${encodeURIComponent(name)}`, { method: "PUT", body: JSON.stringify({ value }) });
  deleteToolVariable = (name: string) =>
    this.request<void>(`/tool-environment/${encodeURIComponent(name)}`, { method: "DELETE", body: JSON.stringify({}) });

  // Companion enrollment & devices (admin) — real routes: GET /companion/devices, DELETE /companion/devices/{id}, PUT .../{id}/allow-oauth.
  companionDevices = () => this.list<CompanionDevice>("/companion/devices");
  createCompanionEnrollment = () =>
    this.request<CreateCompanionEnrollmentResponse>("/companion-enrollments", { method: "POST", body: JSON.stringify({}) });
  setDeviceAllowOAuth = (id: string, allow: boolean) =>
    this.request<CompanionDevice>(`/companion/devices/${encodeURIComponent(id)}/allow-oauth`, {
      method: "PUT",
      body: JSON.stringify({ allow, allow_provider_oauth: allow }),
    });
  revokeCompanionDevice = (id: string) =>
    this.request<void>(`/companion/devices/${encodeURIComponent(id)}`, { method: "DELETE", body: JSON.stringify({}) });

  // Phone notifications (ntfy) — admin
  ntfyConfig = () => this.request<NtfyConfig>("/ntfy");
  setNtfyEnabled = (enabled: boolean) => this.request<NtfyConfig>("/ntfy", { method: "PUT", body: JSON.stringify({ enabled }) });

  knowledgeIndexSetupOptions = () => this.list<IndexSetupOption>("/knowledge/index-setup-options");
  knowledgeGetIndexSetup = () => this.request<{ choice: string }>("/knowledge/index-setup");

  knowledgeSetIndexSetup = (choice: string) => this.request<{ choice: string }>("/knowledge/index-setup", { method: "PUT", body: JSON.stringify({ choice }) });

  publicStatus = () => this.request<PublicStatusResponse>("/public/status");

  doctor = () => this.request<DoctorReport>("/doctor");

  hermesMCPToken = () => this.request<{ token: string }>("/hermes/mcp-token");

  rotateHermesMCPToken = () => this.request<{ token: string }>("/hermes/mcp-token/rotate", { method: "POST", body: JSON.stringify({}) });

  // Server SSH access (admin user authorized_keys)
  systemSSHKeys = () => this.request<SystemSSHKeysResponse>("/system/ssh-keys");
  addSystemSSHKeys = (input: AddSSHKeysRequest) =>
    this.request<AddSSHKeysResponse>("/system/ssh-keys", { method: "POST", body: JSON.stringify(input) });
  deleteSystemSSHKey = (input: DeleteSSHKeyRequest) =>
    this.request<void>(`/system/ssh-keys`, { method: "DELETE", body: JSON.stringify(input) });

  knowledgePinnedModels = () => this.list<ModelInfo>("/knowledge/pinned-models");

  knowledgeGetConsent = (
    input: { provider: string; principal?: string },
  ): Promise<KnowledgeConsent> => {
    const provider = input.provider;
    const principal = input.principal ?? "default";
    const qs = `principal=${encodeURIComponent(principal)}&provider=${encodeURIComponent(provider)}`;
    return this.request<KnowledgeConsent>(`/knowledge/consent?${qs}`);
  };

  knowledgeSetConsent = (
    input: { provider: string; granted: boolean; principal?: string },
  ): Promise<KnowledgeConsent> => {
    const provider = input.provider;
    const granted = input.granted;
    const principal = input.principal ?? "default";
    return this.request<KnowledgeConsent>("/knowledge/consent", {
      method: "PUT",
      body: JSON.stringify({ principal, provider, granted }),
    });
  };
  async streamEvents(signal: AbortSignal, onEvent: (event: ControlEvent) => void, lastEventId?: string): Promise<void> {
    const token = this.getToken();
    const url = lastEventId ? `${API_ROOT}/events/stream?lastEventId=${encodeURIComponent(lastEventId)}` : `${API_ROOT}/events/stream`;
    const headers: Record<string, string> = { Accept: "text/event-stream" };
    if (token) headers.Authorization = `Bearer ${token}`;
    if (lastEventId) headers["Last-Event-ID"] = lastEventId;
    const response = await fetch(url, { headers, signal });
    if (!response.ok || !response.body) {
      if (response.status === 401) window.dispatchEvent(new Event("omahab:unauthorized"));
      throw new ApiError("The live event stream is unavailable.", "event_stream_unavailable", response.status);
    }

    const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
    let buffer = "";
    while (!signal.aborted) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += value;
      const frames = buffer.split(/\r?\n\r?\n/);
      buffer = frames.pop() ?? "";
      for (const frame of frames) {
        const data = frame
          .split(/\r?\n/)
          .filter((line) => line.startsWith("data:"))
          .map((line) => line.slice(5).trimStart())
          .join("\n");
        if (!data) continue;
        const event = parseControlEvent(data);
        // Malformed external frames are isolated; the next frame still delivers.
        if (!event) continue;
        try {
          onEvent(event);
        } catch (err) {
          throw new EventConsumerError("The live event consumer failed.", { cause: err });
        }
      }
    }
  }
}
