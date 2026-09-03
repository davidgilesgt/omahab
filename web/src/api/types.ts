export type ID = string;
export type Health = "unknown" | "healthy" | "degraded" | "unhealthy";
export type Exposure = "private" | "shared" | "public";

export interface ApiErrorEnvelope {
  error: { code: string; message: string };
}

export interface Status {
  instance_id: ID;
  version: string;
  health: Health;
  started_at: string;
  now: string;
}

export interface Application {
  id: ID;
  name: string;
  bundle_id: string;
  image: string;
  digest: string;
  hostname: string;
  exposure: Exposure;

  health: Health;
  desired_state: string;
  observed_state: string;
  installed_at?: string;
  updated_at: string;
}

export interface CatalogBundle {
  id: string;
  name: string;
  image: string;
  architectures: string[];
  default_exposure: Exposure;
  max_exposure: Exposure;
  memory_mb?: number;
  installed: boolean;
  /** "systemd" = native NixOS service (auto-installed, image-versioned); "compose" = Docker Compose. */
  runtime?: string;
}
export interface ExposureState {
  resource_type: "application" | "project";
  resource_id: ID;
  hostname: string;
  exposure: Exposure;
  updated_at: string;
}

export interface Project {
  id: ID;
  slug: string;
  name: string;
  repository_url: string;
  bot_profile_id: string;
  exposure: Exposure;
  hostname: string;
  created_at: string;
  updated_at: string;
}

export interface Release {
  id: ID;
  project_id: ID;
  commit: string;
  digest: string;
  status: string;
  active: boolean;
  created_at: string;
  updated_at: string;
}

export interface Backup {
  id: ID;
  repository: string;
  snapshot_id?: string;
  status: string;
  verified_at?: string;
  started_at: string;
  finished_at?: string;
  error?: string;
}

export interface ControlEvent {
  id: ID;
  type: string;
  severity: string;
  resource_id?: ID;
  message: string;
  data?: Record<string, unknown>;
  read_at?: string;
  created_at: string;
}

export interface SyncFolder {
  id: ID;
  name: string;
  server_path: string;
  share_with_ai: boolean;
  health: Health;
  created_at: string;
  updated_at: string;
}

export interface Workspace {
  id: ID;
  project_id: ID;
  branch: string;
  agent: string;
  status: string;
  last_active_at: string;
  expires_at?: string | null;
  created_at: string;
}

export interface User {
  id: ID;
  email: string;
  name: string;
  groups: string[];
  disabled: boolean;
  created_at: string;
  updated_at: string;
  enrollment_url?: string | null;
  enrollment_expires_at?: string | null;
  pocket_user_id?: string | null;
}

export interface ProviderCredential {
  id: ID;
  provider: "openai" | "anthropic" | "openrouter" | "chatgpt" | "xai" | string;
  name: string;
  kind: "api_key" | "oauth" | string;
  status: string;
  configured: boolean;
  managed_by: "omahab" | "litellm" | string;
  external_ref?: string | null;
  entitlement?: string | null;
  expires_at?: string | null;
  updated_at: string;
}

export type ModelAliasName = "omahab/fast" | "omahab/balanced" | "omahab/reasoning" | "omahab/embedding";

export interface ModelAlias {
  name: ModelAliasName;
  credential_id: ID;
  model: string;
  fallback_order?: string[] | null;
  created_at: string;
  updated_at: string;
}

export interface ModelKey {
  id: ID;
  name: string;
  key_prefix: string;
  owner_kind: "hermes" | "device" | "harness";
  owner_id: string;
  scopes: ModelAliasName[];
  rpm?: number | null;
  tpm?: number | null;
  concurrency?: number | null;
  budget?: number | null;
  created_at: string;
  expires_at?: string | null;
}

export interface CreateModelKeyResponse extends ModelKey {
  key: string;
}

export interface OAuthSession {
  id: string;
  provider: "chatgpt" | "xai" | string;
  flow: "device_code" | "loopback";
  verification_url: string;
  user_code?: string | null;
  callback_port?: number | null;
  expires_at: string;
  status: "pending" | "connected" | "denied" | "expired" | "error";
}

export interface RecoverySession {
  expires_at: string;
  login_url?: string | null;
  code?: string | null;
}

export interface Instance {
  domain: string;
  tailnet: string;
  tailscale_ip: string;
  assistant_name: string;
  assistant_slug: string;
  created_at: string;
}

export interface Secret {
  id: ID;
  scope: string;
  name: string;
  version: number;
  created_at: string;
  updated_at: string;
}

export interface SetupCheck {
  id: string;
  label: string;
  owner: "operator" | "system";
  status: "ok" | "pending" | "failed" | "skipped";
  detail?: string;
  action?: string;
  apps?: { bundle_id: string; status: string }[];
  passkey_count?: number;
  target?: number;
}

export interface SetupStatus {
  state: "waiting_for_cloudflare" | "reconciling" | "attention" | "complete";
  checks: SetupCheck[];
}

export interface ListEnvelope<T> {
  items: T[];
}

export interface IndexSetupOption {
  id: string;
  label: string;
  description: string;
  model_alias?: string | null;
}

export interface ModelInfo {
  alias: string;
  name: string;
  model_id: string;
  license: string;
  size_bytes: number;
  expected_memory_mb: number;
  dimensions?: number;
  max_sequence_length?: number;
  artifact_path?: string;
}

export interface KnowledgeConsent {
  principal: string;
  provider: string;
  granted: boolean;
}

export interface ToolVariableMeta {
  name: string;
  version: number;
  updated_at: string;
}

export interface CompanionDevice {
  id: ID;
  name: string;
  device_token_prefix?: string | null;
  allow_provider_oauth: boolean;
  granted?: boolean;
  last_sync_at?: string | null;
  revoked_at?: string | null;
  created_at: string;
  updated_at: string;
}

export interface CompanionEnrollment {
  id: ID;
  code: string;
  expires_at: string;
  created_at: string;
  consumed_at?: string | null;
}

export interface CreateCompanionEnrollmentResponse {
  id: string;
  code: string;
  expires_at: string;
}
