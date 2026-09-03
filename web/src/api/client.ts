import type {
  ApiErrorEnvelope,
  Application,
  Backup,
  CatalogBundle,
  CompanionDevice,
  ControlEvent,
  CreateCompanionEnrollmentResponse,
  CreateModelKeyResponse,
  Exposure,
  ExposureState,
  IndexSetupOption,
  Instance,
  KnowledgeConsent,
  ListEnvelope,
  ModelAlias,
  ModelAliasName,
  ModelInfo,
  ModelKey,
  OAuthSession,
  Project,
  ProviderCredential,
  RecoverySession,
  Release,
  Secret,
  SetupStatus,
  Status,
  SyncFolder,
  ToolVariableMeta,
  User,
  Workspace,
} from "./types";

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

export function isApiError(error: unknown): error is ApiError {
  return error instanceof ApiError;
}

export function isUnauthorizedError(error: unknown): boolean {
  return error instanceof ApiError && error.status === 401;
}

function isEnvelope<T>(value: T[] | ListEnvelope<T>): value is ListEnvelope<T> {
  return !Array.isArray(value);
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
  setExposure = (resource: "applications" | "projects", id: string, exposure: Exposure, confirmation?: string) =>
    this.request<ExposureState>(`/${resource}/${encodeURIComponent(id)}/exposure`, {
      method: "PUT",
      body: JSON.stringify({ exposure, confirmation }),
    });

  projects = () => this.list<Project>("/projects");
  releases = (projectId: string) => this.list<Release>(`/projects/${encodeURIComponent(projectId)}/releases`);
  rollbackRelease = (projectId: string, releaseId: string) =>
    this.request<Release>(`/projects/${encodeURIComponent(projectId)}/releases/${encodeURIComponent(releaseId)}/rollback`, {
      method: "POST",
      body: JSON.stringify({}),
    });

  backups = () => this.list<Backup>("/backups");
  createBackup = () => this.request<Backup>("/backups", { method: "POST", body: JSON.stringify({}) });
  verifyBackup = (id: string) => this.request<Backup>(`/backups/${encodeURIComponent(id)}/verify`, { method: "POST", body: JSON.stringify({}) });

  events = () => this.list<ControlEvent>("/events");
  markEventRead = (id: string) => this.request<ControlEvent>(`/events/${encodeURIComponent(id)}/read`, { method: "POST", body: JSON.stringify({}) });

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
  beginRecovery = (id: string) =>
    this.request<RecoverySession>(`/users/${encodeURIComponent(id)}/recovery`, { method: "POST", body: JSON.stringify({}) });
  issueEnrollment = (id: string) => this.request<User>(`/users/${encodeURIComponent(id)}/enrollment`, { method: "POST", body: JSON.stringify({}) });

  instance = () => this.request<Instance>("/instance");
  updateInstance = (input: { domain: string; assistant_name?: string }) =>
    this.request<Instance>("/instance", { method: "PUT", body: JSON.stringify(input) });
  createSecret = (input: { scope: string; name: string; value: string }) =>
    this.request<Secret>("/secrets", { method: "POST", body: JSON.stringify(input) });

  setup = () => this.request<SetupStatus>("/setup");
  reconcileSetup = () => this.request<void>("/setup/reconcile", { method: "POST", body: JSON.stringify({}) });
  setupWoodpecker = (input: { username: string; token: string }) =>
    this.request<{ status: string }>("/setup/woodpecker", { method: "PUT", body: JSON.stringify(input) });

  providerCredentials = () => this.list<ProviderCredential>("/provider-credentials");
  createProviderCredential = (input: { provider: string; kind: string; value: string; name?: string }) =>
    this.request<ProviderCredential>("/provider-credentials", { method: "POST", body: JSON.stringify(input) });
  revokeProvider = (id: string) =>
    this.request<void>(`/provider-credentials/${encodeURIComponent(id)}`, { method: "DELETE", body: JSON.stringify({}) });

  // Model gateway (LiteLLM) — aliases
  modelAliases = () => this.list<ModelAlias>("/model-aliases");
  setModelAlias = (name: ModelAliasName, input: { credential_id: string; model: string; fallback_order?: string[] }) =>
    this.request<ModelAlias>(`/model-aliases/${encodeURIComponent(name)}`, { method: "PUT", body: JSON.stringify(input) });

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

  // Companion enrollment & devices (admin)
  companionDevices = () => this.list<CompanionDevice>("/companion-devices");
  createCompanionEnrollment = () =>
    this.request<CreateCompanionEnrollmentResponse>("/companion-enrollments", { method: "POST", body: JSON.stringify({}) });
  updateCompanionDevice = (id: string, input: { allow_provider_oauth?: boolean; granted?: boolean }) =>
    this.request<CompanionDevice>(`/companion-devices/${encodeURIComponent(id)}`, { method: "PATCH", body: JSON.stringify(input) });
  revokeCompanionDevice = (id: string) =>
    this.request<void>(`/companion-devices/${encodeURIComponent(id)}`, { method: "DELETE", body: JSON.stringify({}) });
  toolEnvironmentDevices = () => this.list<CompanionDevice>("/tool-environment/devices");
  setToolEnvironmentGrant = (deviceId: string, granted: boolean) =>
    this.request<void>(`/tool-environment/grants/${encodeURIComponent(deviceId)}`, { method: granted ? "PUT" : "DELETE", body: JSON.stringify({}) });

  knowledgeIndexSetupOptions = () => this.list<IndexSetupOption>("/knowledge/index-setup-options");

  knowledgePinnedModels = () => this.list<ModelInfo>("/knowledge/pinned-models");

  knowledgeGetConsent = (
    providerOrInput: string | { provider: string; principal?: string },
    principalMaybe?: string,
  ): Promise<KnowledgeConsent> => {
    let provider: string;
    let principal = "default";
    if (typeof providerOrInput === "string") {
      if (principalMaybe !== undefined && typeof principalMaybe === "string") {
        // Handle both (provider, principal) and (principal, provider) by detecting if first is a known principal placeholder
        // Prefer provider-first as per task, but also support principal-first for backend shape
        const looksLikePrincipal = providerOrInput === "default" || providerOrInput.includes("@");
        const secondLooksLikePrincipal = principalMaybe === "default" || principalMaybe.includes("@");
        if (looksLikePrincipal && !secondLooksLikePrincipal) {
          principal = providerOrInput;
          provider = principalMaybe;
        } else if (!looksLikePrincipal && secondLooksLikePrincipal) {
          provider = providerOrInput;
          principal = principalMaybe;
        } else {
          // Default to provider first to match task description
          provider = providerOrInput;
          principal = principalMaybe;
        }
      } else {
        provider = providerOrInput;
      }
    } else {
      provider = providerOrInput.provider;
      principal = providerOrInput.principal ?? "default";
    }
    const qs = `principal=${encodeURIComponent(principal)}&provider=${encodeURIComponent(provider)}`;
    return this.request<KnowledgeConsent>(`/knowledge/consent?${qs}`);
  };

  knowledgeSetConsent = (
    providerOrInput: string | { provider: string; granted: boolean; principal?: string },
    grantedMaybe?: boolean,
    principalMaybe?: string,
  ): Promise<KnowledgeConsent> => {
    let provider: string;
    let granted: boolean;
    let principal = "default";
    if (typeof providerOrInput === "object") {
      provider = providerOrInput.provider;
      granted = providerOrInput.granted;
      principal = providerOrInput.principal ?? "default";
    } else if (typeof grantedMaybe === "boolean") {
      provider = providerOrInput;
      granted = grantedMaybe;
      if (typeof principalMaybe === "string") principal = principalMaybe;
    } else if (typeof grantedMaybe === "string" && typeof principalMaybe === "boolean") {
      // (principal, provider, granted) shape
      principal = providerOrInput;
      provider = grantedMaybe as unknown as string;
      granted = principalMaybe as unknown as boolean;
    } else {
      provider = providerOrInput;
      granted = false;
    }
    return this.request<KnowledgeConsent>("/knowledge/consent", {
      method: "PUT",
      body: JSON.stringify({ principal, provider, granted }),
    });
  };

  // Backwards compatible aliases that mirror openapi operationIds
  getKnowledgeConsent = this.knowledgeGetConsent;
  setKnowledgeConsent = this.knowledgeSetConsent;
  getKnowledgeIndexSetupOptions = this.knowledgeIndexSetupOptions;
  getKnowledgePinnedModels = this.knowledgePinnedModels;

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
        try {
          onEvent(JSON.parse(data) as ControlEvent);
        } catch {
          // A malformed event must not stop subsequent control-plane events.
        }
      }
    }
  }
}
