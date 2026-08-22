package tui

import (
	"github.com/charmbracelet/lipgloss"
	"github.com/omahab/omahab/internal/installer"
)

// Caps is the TUI-local capability ladder, matched to installer.Capabilities.
type Caps = installer.Capabilities

// Accent tokens match web/src/styles.css exactly.
// Light: #4C5B36, Dark: #B2C27D
var Accent = lipgloss.AdaptiveColor{Light: "#4C5B36", Dark: "#B2C27D"}

// Positive / Warning / Negative palettes mirror web .status-*
// Light mode values from :root, dark from :root[data-theme="dark"].
var (
	PositiveFG = lipgloss.AdaptiveColor{Light: "#35613f", Dark: "#9bc39c"}
	PositiveBG = lipgloss.AdaptiveColor{Light: "#dfeade", Dark: "#263b29"}
	WarningFG  = lipgloss.AdaptiveColor{Light: "#765b14", Dark: "#ddc46f"}
	WarningBG  = lipgloss.AdaptiveColor{Light: "#f2e8c7", Dark: "#41381d"}
	NegativeFG = lipgloss.AdaptiveColor{Light: "#8b332b", Dark: "#e6a09a"}
	NegativeBG = lipgloss.AdaptiveColor{Light: "#f3dfdc", Dark: "#472925"}
)

// Chip styles — used for PASS/WARN/FAIL rendering.
// They are AdaptiveColor aware; when ColorEnabled is false callers should
// bypass styling and emit plain ASCII like [PASS].
var (
	PassChip = lipgloss.NewStyle().Foreground(PositiveFG).Background(PositiveBG).Padding(0, 1).Bold(true)
	WarnChip = lipgloss.NewStyle().Foreground(WarningFG).Background(WarningBG).Padding(0, 1).Bold(true)
	FailChip = lipgloss.NewStyle().Foreground(NegativeFG).Background(NegativeBG).Padding(0, 1).Bold(true)

	AccentStyle = lipgloss.NewStyle().Foreground(Accent).Bold(true)
	MutedStyle  = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#66695d", Dark: "#aaa99e"})
)

func chipForLevel(level string, caps Caps) string {
	// caps is local alias to avoid import cycle; defined in renderer.go
	// but we also support installer.Capabilities via interface.
	// This helper is called with hasColor bool.
	return level
}

// RenderChip returns a styled or plain chip for a level string.
// levels: "pass", "warn", "fail" (case-insensitive)
func RenderChip(level string, colorEnabled bool) string {
	switch level {
	case "pass", "PASS", "ok", "healthy", "positive":
		if colorEnabled {
			return PassChip.Render(" PASS ")
		}
		return " PASS "
	case "warn", "WARN", "warning", "degraded":
		if colorEnabled {
			return WarnChip.Render(" WARN ")
		}
		return " WARN "
	case "fail", "FAIL", "error", "unhealthy", "negative":
		if colorEnabled {
			return FailChip.Render(" FAIL ")
		}
		return " FAIL "
	default:
		if colorEnabled {
			return lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#66695d", Dark: "#aaa99e"}).Padding(0, 1).Render(" " + level + " ")
		}
		return " " + level + " "
	}
}

// Glyphs with fallback per rendering contract.
// When color is disabled or TERM=dumb / non-TTY, fall back to ASCII.
func GlyphPass(caps Caps) string {
	if caps.IsTTY && caps.ColorEnabled {
		return "●"
	}
	return "[x]"
}

func GlyphFail(caps Caps) string {
	if caps.IsTTY && caps.ColorEnabled {
		return "✗"
	}
	return "[!]"
}

func GlyphWarn(caps Caps) string {
	if caps.IsTTY && caps.ColorEnabled {
		return "◐"
	}
	return "[~]"
}

func GlyphSpinner(caps Caps, frame int) string {
	if !caps.IsTTY || !caps.ColorEnabled {
		return "[.]"
	}
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	return frames[frame%len(frames)]
}

func GlyphStepDone(caps Caps) string {
	if caps.IsTTY && caps.ColorEnabled {
		return "✓"
	}
	return "[x]"
}

func GlyphStepRunning(caps Caps) string {
	if caps.IsTTY && caps.ColorEnabled {
		return "●"
	}
	return "[o]"
}

func GlyphStepPending(caps Caps) string {
	if caps.IsTTY && caps.ColorEnabled {
		return "○"
	}
	return "[ ]"
}

func GlyphDot() string { return "·" }
