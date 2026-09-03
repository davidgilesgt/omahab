package docker

import (
	"errors"
	"fmt"
	"strings"

	"github.com/moby/moby/client"
)

type DescribedError interface {
	error
	Description() string
}

var (
	ErrProxyPortInUse = &describedError{
		msg:         "proxy port conflict",
		description: "Something else is using the web ports on this machine. You'll need to stop that service, and then try deploying again.",
	}
	ErrAppNotStarted = &describedError{
		msg:         "application did not start",
		description: "The application did not start within the time limit. Check the application logs for errors.",
	}
	ErrDockerPermissionDenied = &describedError{
		msg:         "permission denied connecting to Docker",
		description: "Permission denied when connecting to the Docker socket. Run with `sudo`, or add yourself to the `docker` group.",
	}
	ErrDockerNotRunning = &describedError{
		msg:         "cannot connect to Docker",
		description: "Could not connect to Docker. Make sure Docker is installed and the Docker daemon is running.",
	}
)

func ErrorMessage(err error) string {
	if de, ok := errors.AsType[DescribedError](err); ok {
		return de.Description()
	}
	return err.Error()
}

// Private

type describedError struct {
	msg         string
	description string
}

func (e *describedError) Error() string       { return e.msg }
func (e *describedError) Description() string { return e.description }

// Helpers

func isPortConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "bind: address already in use") ||
		strings.Contains(msg, "port is already allocated")
}

func connectionError(err error) error {
	switch {
	case err == nil:
		return nil
	case client.IsErrConnectionFailed(err):
		if strings.Contains(err.Error(), "permission denied") {
			return fmt.Errorf("%w: %w", ErrDockerPermissionDenied, err)
		}
		return fmt.Errorf("%w: %w", ErrDockerNotRunning, err)
	default:
		return err
	}
}
