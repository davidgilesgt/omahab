//go:build linux

package client

import (
	"fmt"

	"github.com/godbus/dbus/v5"
)

// realDBus is the production D-Bus implementation using godbus.
// Only compiled on Linux where systemd user manager is available.
type realDBus struct{}

func (r *realDBus) SetEnvironment(assignments []string) error {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return fmt.Errorf("D-Bus session bus not available: %w", err)
	}
	defer conn.Close()
	obj := conn.Object("org.freedesktop.systemd1", dbus.ObjectPath("/org/freedesktop/systemd1"))
	call := obj.Call("org.freedesktop.systemd1.Manager.SetEnvironment", 0, assignments)
	if call.Err != nil {
		return fmt.Errorf("SetEnvironment: %w", call.Err)
	}
	return nil
}

func (r *realDBus) UnsetEnvironment(names []string) error {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return fmt.Errorf("D-Bus session bus not available: %w", err)
	}
	defer conn.Close()
	obj := conn.Object("org.freedesktop.systemd1", dbus.ObjectPath("/org/freedesktop/systemd1"))
	call := obj.Call("org.freedesktop.systemd1.Manager.UnsetEnvironment", 0, names)
	if call.Err != nil {
		return fmt.Errorf("UnsetEnvironment: %w", call.Err)
	}
	return nil
}
