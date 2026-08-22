package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/installer"
)

// SecondSessionGate renders a live 10-minute rollback countdown polling
// ConfirmSecondSession. It is inline (no alt-screen) and leaves a readable
// transcript if SSH drops.
//
// The model poll interval is 1s. Caller should Tick() every second and call
// Poll() which in turn calls probes.SecondSessionProbe via installer.ConfirmSecondSession.
// For pure rendering / testing the model exposes Remaining() and View().
type SecondSessionGate struct {
	caps      Caps
	startedAt time.Time
	deadline time.Time
	now       func() time.Time
	confirmed bool
	failed    string
	polls     int
}

// NewSecondSessionGate creates a gate starting now, 10m deadline.
func NewSecondSessionGate(caps Caps) *SecondSessionGate {
	now := time.Now()
	return &SecondSessionGate{
		caps:      caps,
		startedAt: now,
		deadline: now.Add(10 * time.Minute),
		now:       time.Now,
	}
}

// NewSecondSessionGateAt is test helper with fixed start.
func NewSecondSessionGateAt(caps Caps, start time.Time) *SecondSessionGate {
	return &SecondSessionGate{
		caps:      caps,
		startedAt: start,
		deadline: start.Add(10 * time.Minute),
		now:       time.Now,
	}
}

func (g *SecondSessionGate) SetNow(fn func() time.Time) { g.now = fn }

// Remaining returns duration until deadline, clamped at 0.
func (g *SecondSessionGate) Remaining() time.Duration {
	var d time.Duration
	if g.now != nil {
		d = g.deadline.Sub(g.now())
	} else {
		d = time.Until(g.deadline)
	}
	if d < 0 {
		return 0
	}
	return d.Truncate(time.Second)
}
// IsExpired reports whether countdown reached zero.
func (g *SecondSessionGate) IsExpired() bool { return g.Remaining() <= 0 }

// Tick advances spinner frame.
func (g *SecondSessionGate) Tick() { g.polls++ }

// Poll attempts ConfirmSecondSession via probes. On success marks confirmed.
// Returns true if confirmed.
func (g *SecondSessionGate) Poll(ctx context.Context, probes installer.Probes) (bool, error) {
	g.polls++
	if err := installer.ConfirmSecondSession(ctx, probes); err != nil {
		// Normalize not-confirmed vs error
		if strings.Contains(err.Error(), "second session not confirmed") || strings.Contains(err.Error(), "no second SSH") {
			g.failed = ""
			return false, nil
		}
		g.failed = err.Error()
		return false, err
	}
	g.confirmed = true
	g.failed = ""
	return true, nil
}

// View renders inline gate line with countdown and spinner.
// It does not clear screen; caller prints it and overwrites on next tick
// via carriage return or new line for serial safety we use newline.
func (g *SecondSessionGate) View() string {
	rem := g.Remaining()
	var glyph string
	if g.confirmed {
		glyph = GlyphStepDone(g.caps)
		return fmt.Sprintf("  %s second session confirmed — rollback cancelled (%s remaining)", glyph, rem)
	}
	if g.IsExpired() {
		glyph = GlyphFail(g.caps)
		return fmt.Sprintf("  %s rollback window expired — sshd will revert", glyph)
	}
	// Running state: show countdown with spinner frame based on polls
	sp := GlyphSpinner(g.caps, g.polls)
	// Format remaining as mm:ss
	mm := int(rem.Minutes())
	ss := int(rem.Seconds()) % 60
	countdown := fmt.Sprintf("%02d:%02d", mm, ss)
	// When caps non-tty we don't animate, just show static
	line := fmt.Sprintf("  %s SSH hardening staged — rollback in %s — open a second SSH session then press Enter", sp, countdown)
	if g.failed != "" {
		line += fmt.Sprintf(" (%s)", g.failed)
	}
	// Ensure line not too long for serial console; truncate to 78?
	if len(line) > 78 && g.caps.Width > 0 && len(line) > g.caps.Width {
		line = line[:g.caps.Width]
	}
	return line
}

// SecondSessionLoop runs the interactive countdown, polling every second,
// handling Enter/abort input via provided channel.
// It writes UI to w via renderer conceptually, but this helper is used by
// the installer CLI to render in-place countdown.
// For simplicity the loop is implemented in renderer.go; this file provides
// the model only.
