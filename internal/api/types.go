package api

import "github.com/omahab/omahab/internal/apitypes"

// Request/response types are hand-written in internal/api/types.go (canonical in internal/apitypes)
// and mirrored in web/src/api/types.ts; OpenAPI is descriptive.

type Pagination = apitypes.Pagination
type EventFilter = apitypes.EventFilter
type ExposureState = apitypes.ExposureState
type ProviderCredential = apitypes.ProviderCredential
type ModelAlias = apitypes.ModelAlias
type SetModelAliasRequest = apitypes.SetModelAliasRequest
type ModelKey = apitypes.ModelKey
type CreateModelKeyRequest = apitypes.CreateModelKeyRequest
type OAuthSession = apitypes.OAuthSession
type StartProviderOAuthRequest = apitypes.StartProviderOAuthRequest
type ForwardProviderOAuthCallbackRequest = apitypes.ForwardProviderOAuthCallbackRequest
type CompanionDevice = apitypes.CompanionDevice
type CompanionEnrollment = apitypes.CompanionEnrollment
type EnrollCompanionResponse = apitypes.EnrollCompanionResponse
type UpdateCompanionDeviceRequest = apitypes.UpdateCompanionDeviceRequest
type NtfyConfig = apitypes.NtfyConfig
type SetNtfyRequest = apitypes.SetNtfyRequest
type ToolEnvEntry = apitypes.ToolEnvEntry
type PutToolEnvRequest = apitypes.PutToolEnvRequest
type ToolEnvListResponse = apitypes.ToolEnvListResponse
type RecoverySession = apitypes.RecoverySession
type CreateProjectRequest = apitypes.CreateProjectRequest
type UpdateProjectRequest = apitypes.UpdateProjectRequest
type CreateReleaseRequest = apitypes.CreateReleaseRequest
type UpdateApplicationRequest = apitypes.UpdateApplicationRequest
type ApplicationActionRequest = apitypes.ApplicationActionRequest
type InstallApplicationRequest = apitypes.InstallApplicationRequest
type VerifyCloudflareTokenResult = apitypes.VerifyCloudflareTokenResult
type RecoveryKeyMaterial = apitypes.RecoveryKeyMaterial
type Disk = apitypes.Disk
type ConfigureStorageRequest = apitypes.ConfigureStorageRequest
type CreateBackupRepositoryRequest = apitypes.CreateBackupRepositoryRequest
type CatalogBundle = apitypes.CatalogBundle
type CreateSecretRequest = apitypes.CreateSecretRequest
type UpdateSecretRequest = apitypes.UpdateSecretRequest
type CreateBackupRequest = apitypes.CreateBackupRequest
type UpdateExposureRequest = apitypes.UpdateExposureRequest
type CreateSyncFolderRequest = apitypes.CreateSyncFolderRequest
type UpdateSyncFolderRequest = apitypes.UpdateSyncFolderRequest
type CreateCompanionSyncFolderRequest = apitypes.CreateCompanionSyncFolderRequest
type CreateWorkspaceRequest = apitypes.CreateWorkspaceRequest
type SendWorkspaceRequest = apitypes.SendWorkspaceRequest
type CompanionCreateWorkspaceRequest = apitypes.CompanionCreateWorkspaceRequest
type CreateUserRequest = apitypes.CreateUserRequest
type UpdateUserRequest = apitypes.UpdateUserRequest
type CreateProviderCredentialRequest = apitypes.CreateProviderCredentialRequest
type EmailIngestRequest = apitypes.EmailIngestRequest
type ReleaseTokenResponse = apitypes.ReleaseTokenResponse
type ConfigureMirrorRequest = apitypes.ConfigureMirrorRequest
type MirrorResponse = apitypes.MirrorResponse
type WorkspaceCapabilityResponse = apitypes.WorkspaceCapabilityResponse
type KnowledgeSearchRequest = apitypes.KnowledgeSearchRequest
type KnowledgeUploadRequest = apitypes.KnowledgeUploadRequest
type SetupWoodpeckerRequest = apitypes.SetupWoodpeckerRequest
type SetupStatus = apitypes.SetupStatus
type SetupCheck = apitypes.SetupCheck
type SetupAppStatus = apitypes.SetupAppStatus
