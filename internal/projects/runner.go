package projects

import (
	"context"

	"github.com/omahab/omahab/internal/domain"
)

// TLSMode selects how TLS is terminated for a project deployment.
type TLSMode string

// TLSModeExternal means Caddy owns external TLS: the ONCE proxy listens on
// loopback with internal TLS disabled (design §6.3 edge topology).
const TLSModeExternal TLSMode = "external"

// DeployInput is the complete request the control plane hands to ONCE for
// one project deployment. Secrets are carried by file path only; the path
// may be recorded, the file contents must never appear in logs or JSON.
type DeployInput struct {
	App         string  // ONCE application name (project slug)
	Hostname    string  // host header the loopback proxy routes for this app
	Image       string  // full image reference including @sha256:<digest>
	Port        int     // container HTTP port (default contract: 80)
	HealthPath  string  // container health endpoint (default contract: /up)
	StoragePath string  // host directory mounted at container /storage
	ProxyBind   string  // loopback address the ONCE proxy listens on (e.g. 127.0.0.1:8080)
	TLS         TLSMode // TLSModeExternal for the default edge topology
	SecretsFile string  // projected per-project secrets file path; contents never logged
}

// DeployResult reports runner-observed deployment facts.
type DeployResult struct {
	Version string // runner-reported deployment version, when available
}

// HealthInput asks the runner to probe a deployed project through the
// loopback proxy.
type HealthInput struct {
	App       string // project slug; used for --app in status fallback (once.go:133)
	ProxyBind string
	Hostname  string
	Port      int
	Path      string
}

// HealthResult reports one health probe.
type HealthResult struct {
	Healthy bool
	Detail  string // non-secret diagnostic detail for failures
}

// UndeployInput removes one project's runtime deployment.
type UndeployInput struct {
	App      string
	Hostname string
}

// ONCERunner drives the omahab-once fork. Implementations must:
//
//   - expose the proxy on the supplied loopback bind address with internal
//     TLS disabled (external mode, design §6.3);
//   - consume secrets only from the supplied secrets-file path;
//   - leave the previously active version serving when a new version fails
//     before becoming healthy (retain-and-switch semantics);
//   - never log secret values, only the file path;
//   - surface machine-readable status and probe errors without tearing down
//     state on probe failure.
type ONCERunner interface {
	Deploy(ctx context.Context, in DeployInput) (DeployResult, error)
	Health(ctx context.Context, in HealthInput) (HealthResult, error)
	Undeploy(ctx context.Context, in UndeployInput) error
}

// ReleaseTokenVerifier authorizes Woodpecker-initiated releases with a
// narrowly scoped per-project release token. Woodpecker holds one token per
// project and never a host SSH key or Omahab administrator credential
// (design §6.4); implementations must not include the presented token in
// returned errors. One repository equals one project.
//
// VerifyReleaseToken returns nil when token authorizes a release for
// projectID. Any non-nil error is treated identically by the service: the
// call is rejected with ErrUnauthorized and the underlying reason is not
// exposed to the caller, guaranteeing the token never appears in errors or
// logs.
type ReleaseTokenVerifier interface {
	VerifyReleaseToken(ctx context.Context, projectID domain.ID, token string) error
}

// EventRecorder receives normalized control-plane events. Recording is
// best-effort: a recorder failure must not fail the operation that produced
// the event, so implementations own their durability and diagnostics. Event
// payloads carry identifiers and digests only, never secret values.
type EventRecorder interface {
	Record(ctx context.Context, event domain.Event) error
}
