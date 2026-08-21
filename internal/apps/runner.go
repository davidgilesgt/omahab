package apps

import (
	"context"
	"io"

	"github.com/omahab/omahab/internal/domain"
)

// Runner performs the observable side effects of application lifecycle
// transitions. The production implementation drives Docker Compose through
// an Invoker; tests substitute a fake Runner to assert state transitions
// without touching Docker.
type Runner interface {
	// Deploy creates or replaces the running stack described by spec.
	Deploy(ctx context.Context, app domain.Application, spec DeploySpec) error
	// Start starts an existing, stopped stack.
	Start(ctx context.Context, app domain.Application, spec DeploySpec) error
	// Stop stops a running stack without removing data or definitions.
	Stop(ctx context.Context, app domain.Application, spec DeploySpec) error
	// Remove tears the stack down. Persistent volumes are kept: removing an
	// application and deleting its data are separate operations.
	Remove(ctx context.Context, app domain.Application, spec DeploySpec) error
	// Check observes current health. A failing check is a health result,
	// not an operation error; errors are reserved for misconfiguration.
	Check(ctx context.Context, app domain.Application, spec DeploySpec) (domain.Health, error)
}

// DeploySpec carries everything a Runner needs to act: the exact rendered
// Compose definition (byte-identical to the persisted release), its pinned
// digest, the bundle health check, and the secret environment projection.
// Env values exist only in memory on their way to the runner process; they
// are never persisted, logged, or included in JSON or events.
type DeploySpec struct {
	Compose string
	Digest  string
	Env     []string
	Health  HealthCheck
}

// Invoker runs one external command with an augmented environment. It is
// the single place external processes are spawned, so command invocation is
// observable and testable.
type Invoker interface {
	Run(ctx context.Context, env []string, dir string, stdout, stderr io.Writer, name string, args ...string) error
}
