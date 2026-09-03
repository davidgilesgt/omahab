package client

import (
	"context"
	"os"
	"runtime"
	"time"
)

// HeartbeatOnce collects device identity and reports it via PUT /api/v1/companion/devices/me (C4).
// It is idempotent and best-effort; errors are logged but not propagated to the caller as fatal.
func (d *Daemon) HeartbeatOnce(ctx context.Context) error {
	if d.remote == nil {
		return nil
	}
	hostname, _ := os.Hostname()
	platform := runtime.GOOS
	arch := runtime.GOARCH
	shell := os.Getenv("SHELL")
	ver := Version

	var envRev *int
	var envCount *int
	if d.envManager != nil {
		rev, count, _, _ := d.envManager.Status()
		// Status returns revision, count, syncedAt, errStr — need to capture
		// Use values even if errStr non-empty; they are last known.
		r := rev
		c := count
		envRev = &r
		envCount = &c
	}
	var backupSnap *time.Time
	d.mu.RLock()
	if d.backupLastSnapshot != nil {
		t := *d.backupLastSnapshot
		backupSnap = &t
	}
	d.mu.RUnlock()

	info := DeviceHeartbeatInfo{
		Hostname:           hostname,
		Platform:           platform,
		Arch:               arch,
		ClientVersion:      ver,
		Shell:              shell,
		EnvRevision:        envRev,
		EnvVariableCount:   envCount,
		BackupLastSnapshot: backupSnap,
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return d.remote.UpdateDeviceInfo(ctx2, info)
}

// StartHeartbeatLoop runs HeartbeatOnce immediately and then daily (24h) until ctx cancelled.
// It also runs on a 10-minute safety ticker for tests with short intervals; production uses daily.
// Caller should run this in a goroutine managed by Daemon.wg.
func (d *Daemon) StartHeartbeatLoop(ctx context.Context) {
	// Immediate heartbeat best-effort.
	_ = d.HeartbeatOnce(ctx)
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = d.HeartbeatOnce(ctx)
		}
	}
}

// CollectDeviceInfo is a helper for tests to collect current heartbeat payload without sending.
func (d *Daemon) CollectDeviceInfo() DeviceHeartbeatInfo {
	hostname, _ := os.Hostname()
	platform := runtime.GOOS
	arch := runtime.GOARCH
	shell := os.Getenv("SHELL")
	ver := Version
	var envRev *int
	var envCount *int
	if d.envManager != nil {
		rev, count, _, _ := d.envManager.Status()
		r := rev
		c := count
		envRev = &r
		envCount = &c
	}
	var backupSnap *time.Time
	d.mu.RLock()
	if d.backupLastSnapshot != nil {
		t := *d.backupLastSnapshot
		backupSnap = &t
	}
	d.mu.RUnlock()
	return DeviceHeartbeatInfo{
		Hostname:           hostname,
		Platform:           platform,
		Arch:               arch,
		ClientVersion:      ver,
		Shell:              shell,
		EnvRevision:        envRev,
		EnvVariableCount:   envCount,
		BackupLastSnapshot: backupSnap,
	}
}
