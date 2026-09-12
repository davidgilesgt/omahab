package workspaces

import "errors"

// ErrRunnerUnavailable is returned when the workspace runner binary
// (devpod) is missing or not executable. It maps to HTTP 503 via
// controlplane translateError.
var ErrRunnerUnavailable = errors.New("workspace runner unavailable")
