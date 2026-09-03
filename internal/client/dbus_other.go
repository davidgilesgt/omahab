//go:build !linux && !darwin

package client

import "fmt"

// realDBus stub for other platforms — always returns an error so env sync degrades gracefully.
type realDBus struct{}

func (r *realDBus) SetEnvironment(assignments []string) error {
	return fmt.Errorf("SetEnvironment not supported on %s", "this platform")
}

func (r *realDBus) UnsetEnvironment(names []string) error {
	return fmt.Errorf("UnsetEnvironment not supported on %s", "this platform")
}
