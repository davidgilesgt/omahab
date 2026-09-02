// Package tui provides terminal styling helpers shared by the omahab CLI
// (status strip, doctor checklist chips). The installer-specific TUI
// (stepbar, renderer, forms, checklist) was deleted with the Debian
// installer.
package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Caps is the terminal capability snapshot.
type Caps struct {
	IsTTY        bool
	ColorEnabled bool
}

// Accent tokens match web/src/styles.css exactly.
// Light: #4C5B36, Dark: #B2C27D
var (
	PositiveFG = lipgloss.Color("#15803d")
	PositiveBG = lipgloss.Color("#dcfce7")
	NegativeFG = lipgloss.Color("#b91c1c")
	NegativeBG = lipgloss.Color("#fee2e2")
	WarnFG     = lipgloss.Color("#a16207")
	WarnBG     = lipgloss.Color("#fef3c7")
	NeutralFG  = lipgloss.Color("#525252")
	NeutralBG  = lipgloss.Color("#f5f5f5")

	PassChip = lipgloss.NewStyle().Foreground(PositiveFG).Background(PositiveBG).Padding(0, 1).Bold(true)
	FailChip = lipgloss.NewStyle().Foreground(NegativeFG).Background(NegativeBG).Padding(0, 1).Bold(true)
	WarnChip = lipgloss.NewStyle().Foreground(WarnFG).Background(WarnBG).Padding(0, 1).Bold(true)
	InfoChip = lipgloss.NewStyle().Foreground(NeutralFG).Background(NeutralBG).Padding(0, 1)
)

// HealthChip renders a pass/warn/fail chip for a health status.
func HealthChip(status string) string {
	switch strings.ToLower(status) {
	case "healthy", "ok", "pass", "positive":
		return PassChip.Render(" PASS ")
	case "degraded", "warn", "warning":
		return WarnChip.Render(" WARN ")
	case "unhealthy", "fail", "error", "negative":
		return FailChip.Render(" FAIL ")
	default:
		return InfoChip.Render(" · ")
	}
}

// CompactStatusStrip renders a one-line status strip for `omahab status`.
func CompactStatusStrip(health string, caps Caps) string {
	if !caps.IsTTY || !caps.ColorEnabled {
		return "status: " + health
	}
	return HealthChip(health) + " " + health
}

// DoctorCheckView is a presentation-level check for doctor.
type DoctorCheckView struct {
	Name    string
	Status  string
	Message string
	Detail  string
}

// RenderDoctorChecklist renders health checks as chips with messages.
func RenderDoctorChecklist(checks []DoctorCheckView, caps Caps) string {
	var b strings.Builder
	for _, dc := range checks {
		if caps.IsTTY && caps.ColorEnabled {
			b.WriteString(HealthChip(dc.Status))
			b.WriteByte(' ')
		}
		b.WriteString(dc.Name)
		if dc.Message != "" {
			b.WriteString(" — ")
			b.WriteString(dc.Message)
		}
		b.WriteByte('\n')
		if dc.Detail != "" {
			b.WriteString("    ")
			b.WriteString(dc.Detail)
			b.WriteByte('\n')
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ResolveCaps computes capabilities from stdio state and env.
func ResolveCaps(isTTY bool, term, noColorEnv string) Caps {
	caps := Caps{IsTTY: isTTY}
	if isTTY {
		if noColorEnv == "" && term != "" && term != "dumb" {
			caps.ColorEnabled = true
		}
	}
	return caps
}
