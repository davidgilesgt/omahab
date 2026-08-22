package tui

import (
	"fmt"
	"strings"

	"github.com/omahab/omahab/internal/installer"
)

// PreflightChecklist renders preflight checks with spinner on running probe.
// It is pure state -> string for golden-file testing.
// Final frame freezes (no spinner) once Freeze() is called or StepFinished received.
type PreflightChecklist struct {
	caps       Caps
	checks     []installer.CheckResult
	running    string
	frozen     bool
	spinnerIdx int
}

// NewPreflightChecklist creates a checklist with given capabilities.
func NewPreflightChecklist(caps Caps) *PreflightChecklist {
	return &PreflightChecklist{caps: caps}
}

// Feed ingests an installer Event. Returns true if model changed.
func (m *PreflightChecklist) Feed(e installer.Event) bool {
	switch v := e.(type) {
	case installer.PreflightCheck:
		m.checks = append(m.checks, v.Result)
		// clear running if matches
		if m.running == v.Result.Name {
			m.running = ""
		}
		return true
	case installer.StepStarted:
		if v.Step == installer.StepPreflight {
			m.running = "preflight"
			m.frozen = false
		}
		return true
	case installer.StepFinished:
		if v.Result.Step == installer.StepPreflight {
			m.frozen = true
			m.running = ""
		}
		return true
	}
	return false
}

// AddResult adds a check result directly (for doctor reuse).
func (m *PreflightChecklist) AddResult(r installer.CheckResult) {
	m.checks = append(m.checks, r)
}

// SetRunning sets the name of currently running probe for spinner.
func (m *PreflightChecklist) SetRunning(name string) {
	if !m.frozen {
		m.running = name
	}
}

// Freeze marks the checklist as final (no spinner).
func (m *PreflightChecklist) Freeze() { m.frozen = true; m.running = "" }

// Tick advances spinner frame (call on timer tick while not frozen).
func (m *PreflightChecklist) Tick() {
	if !m.frozen {
		m.spinnerIdx++
	}
}

// View renders the checklist to string. Inline, no alt-screen.
func (m *PreflightChecklist) View() string {
	var b strings.Builder
	for _, c := range m.checks {
		var glyph string
		var chip string
		switch c.Level {
		case installer.LevelPass:
			glyph = GlyphPass(m.caps)
			chip = RenderChip("pass", m.caps.ColorEnabled)
		case installer.LevelWarn:
			glyph = GlyphWarn(m.caps)
			chip = RenderChip("warn", m.caps.ColorEnabled)
		case installer.LevelFail:
			glyph = GlyphFail(m.caps)
			chip = RenderChip("fail", m.caps.ColorEnabled)
		default:
			glyph = GlyphWarn(m.caps)
			chip = RenderChip(string(c.Level), m.caps.ColorEnabled)
		}
		// When color disabled, chip is plain " PASS " etc; glyph fallback already ASCII.
		// Render line: glyph + chip + name + message
		extra := ""
		if c.Dirty {
			extra = " [dirty]"
		}
		// Use non-colored fixed layout for test determinism; when color enabled we still include glyph+chip string which contains ANSI.
		// Format: "  <glyph> <chip> name message"
		// For golden tests without color, chip will be plain.
		line := fmt.Sprintf("  %s %s %-16s %s%s", glyph, strings.TrimSpace(chip), c.Name, c.Message, extra)
		b.WriteString(line)
		b.WriteString("\n")
		if c.Remediation != "" {
			b.WriteString(fmt.Sprintf("       -> %s\n", c.Remediation))
		}
	}
	if !m.frozen && m.running != "" {
		sp := GlyphSpinner(m.caps, m.spinnerIdx)
		// Show spinner line for running probe
		b.WriteString(fmt.Sprintf("  %s %-16s %s\n", sp, m.running, "running..."))
	}
	// Trim final newline for consistent golden files? Keep as is but ensure no extra blank.
	s := b.String()
	return s
}

// String implements stringer for tests.
func (m *PreflightChecklist) String() string { return m.View() }

// RenderDoctorChecklist renders health checks through the same checklist visual
// but mapping health statuses to pass/warn/fail chips.
// This is used by `omahab doctor` to reuse the component.
func RenderDoctorChecklist(checks []DoctorCheckView, caps Caps) string {
	m := NewPreflightChecklist(caps)
	for _, dc := range checks {
		level := mapHealthToLevel(dc.Status)
		m.AddResult(installer.CheckResult{
			Name:        dc.Name,
			Level:       level,
			Message:     dc.Message,
			Remediation: dc.Detail,
		})
	}
	m.Freeze()
	return m.View()
}

// DoctorCheckView is a presentation-level check for doctor (adapts apiclient.DoctorCheck).
type DoctorCheckView struct {
	Name    string
	Status  string
	Message string
	Detail  string
}

func mapHealthToLevel(status string) installer.Level {
	switch strings.ToLower(status) {
	case "healthy", "ok", "pass", "positive":
		return installer.LevelPass
	case "degraded", "warn", "warning":
		return installer.LevelWarn
	case "unhealthy", "fail", "error", "negative":
		return installer.LevelFail
	default:
		return installer.LevelWarn
	}
}
