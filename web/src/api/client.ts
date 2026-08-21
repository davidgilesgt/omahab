import type {
  ApiErrorEnvelope,
  Application,
  Backup,
  CatalogBundle,
  ControlEvent,
  Exposure,
  ExposureState,
  ListEnvelope,
  Project,
  ProviderCredential,
  RecoverySession,
  Release,
  Status,
  SyncFolder,
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
  createUser = (input: { name: string; email: string }) => this.request<User>("/users", { method: "POST", body: JSON.stringify(input) });
  setUserDisabled = (id: string, disabled: boolean) =>
    this.request<User>(`/users/${encodeURIComponent(id)}`, { method: "PATCH", body: JSON.stringify({ disabled }) });
  beginRecovery = (id: string) =>
    this.request<RecoverySession>(`/users/${encodeURIComponent(id)}/recovery`, { method: "POST", body: JSON.stringify({}) });

  providerCredentials = () => this.list<ProviderCredential>("/provider-credentials");
  createProviderCredential = (input: { provider: string; kind: string; value: string; name?: string }) =>
    this.request<ProviderCredential>("/provider-credentials", { method: "POST", body: JSON.stringify(input) });
  revokeProvider = (id: string) =>
    this.request<void>(`/provider-credentials/${encodeURIComponent(id)}`, { method: "DELETE", body: JSON.stringify({}) });

  async streamEvents(signal: AbortSignal, onEvent: (event: ControlEvent) => void): Promise<void> {
    const token = this.getToken();
    const response = await fetch(`${API_ROOT}/events/stream`, {
      headers: { Accept: "text/event-stream", ...(token ? { Authorization: `Bearer ${token}` } : {}) },
      signal,
    });
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
