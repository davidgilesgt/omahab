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
}

export interface ProviderCredential {
  id: ID;
  provider: string;
  name: string;
  kind: string;
  status: string;
  configured: boolean;
  entitlement?: string | null;
  expires_at?: string | null;
  updated_at: string;
}

export interface RecoverySession {
  expires_at: string;
  login_url?: string | null;
  code?: string | null;
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
