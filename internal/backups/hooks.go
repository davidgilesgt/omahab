package backups

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// Hook is an application consistency hook supplied by an application
// bundle, such as a database dump before backup or a schema migration check
// after restore. Hook commands must not carry secret values; output is
// persisted for audit after truncation and redaction.
type Hook struct {
	// Application names the bundle the hook belongs to, e.g. "immich".
	Application string
	// Command is the argv to execute.
	Command []string
	// Timeout bounds execution; non-positive means defaultHookTimeout.
	Timeout time.Duration
}

func (h Hook) validate() error {
	if h.Application == "" {
		return errors.New("application is required")
	}
	if len(h.Command) == 0 {
		return errors.New("command is required")
	}
	for _, a := range h.Command {
		if a == "" {
			return errors.New("command contains an empty argument")
		}
	}
	return nil
}

// HookSource supplies the application hooks that must run for a kind. The
// applications controller is the standard source; bundles without state that
// needs consistency simply register no hooks.
type HookSource interface {
	Hooks(ctx context.Context, kind HookKind) ([]Hook, error)
}

// HookOutcome is the result of executing one hook.
type HookOutcome struct {
	// ExitCode is the process exit code, nil when the hook could not be
	// executed at all.
	ExitCode  *int
	Output    string
	Error     error
	StartedAt time.Time
	// FinishedAt is the completion time; StartedAt plus a duration is
	// accepted when implementations cannot report it.
	FinishedAt time.Time
}

// HookRunner executes a single application hook. The context passed to
// RunHook already carries the hook's timeout.
type HookRunner interface {
	RunHook(ctx context.Context, h Hook) HookOutcome
}

// ExecHookRunner executes hooks as subprocesses with combined output
// capture.
type ExecHookRunner struct{}

var _ HookRunner = ExecHookRunner{}

func (ExecHookRunner) RunHook(ctx context.Context, h Hook) HookOutcome {
	start := time.Now()
	if err := h.validate(); err != nil {
		return HookOutcome{Error: err, StartedAt: start, FinishedAt: start}
	}
	cmd := exec.CommandContext(ctx, h.Command[0], h.Command[1:]...)
	output, err := cmd.CombinedOutput()
	out := HookOutcome{Output: string(output), StartedAt: start, FinishedAt: time.Now()}
	switch {
	case ctx.Err() != nil:
		out.Error = fmt.Errorf("hook exceeded its timeout or was canceled: %w", ctx.Err())
	case err != nil:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code := ee.ExitCode()
			out.ExitCode = &code
		} else {
			out.Error = err
		}
	default:
		code := 0
		out.ExitCode = &code
	}
	return out
}

// SecretSource resolves a SecretRef into restic credentials. The standard
// implementation is the secrets controller. Implementations must not log or
// persist resolved values and must not include them in returned errors.
type SecretSource interface {
	Resolve(ctx context.Context, ref SecretRef) (Credentials, error)
}
