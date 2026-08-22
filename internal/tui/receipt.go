package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/installer"
)

// RenderReceipt renders a serial-console-safe receipt, ≤64 columns per line.
// It includes host, version, step durations, and next actions (Tailscale URL,
// dashboard URL, omahab doctor). It is used at install completion and by
// `omahab-install manifest` non-JSON path; --json marshals the same struct.
func RenderReceipt(m installer.Manifest) string {
	return RenderReceiptWithCaps(m, Caps{IsTTY: true, ColorEnabled: false, Width: 64}, "")
}

// RenderReceiptWithCaps allows caller to pass caps and optional tailscale IP
// for dashboard URL. Width is clamped to ≤64 for serial console safety.
func RenderReceiptWithCaps(m installer.Manifest, caps Caps, tailscaleIP string) string {
	width := 64
	if caps.Width > 0 && caps.Width < 64 {
		width = caps.Width
	}
	var b strings.Builder
	// Header
	b.WriteString(headerLine(width))
	b.WriteString(centerPad("Omahab install receipt", width))
	b.WriteString("\n")
	b.WriteString(headerLine(width))

	// Host/version
	host := formatHost(m)
	ver := m.Version
	if ver == "" {
		ver = "unknown"
	}
	when := m.InstalledAt.UTC().Format(time.RFC3339)
	if m.InstalledAt.IsZero() {
		when = time.Now().UTC().Format(time.RFC3339)
	}
	writeKV(&b, "host", host, width)
	writeKV(&b, "version", ver, width)
	writeKV(&b, "installed", when, width)
	if m.Arch != "" {
		writeKV(&b, "arch", m.Arch, width)
	}
	if m.OS.Pretty != "" {
		writeKV(&b, "os", m.OS.Pretty, width)
	} else if m.OS.ID != "" {
		writeKV(&b, "os", fmt.Sprintf("%s %s", m.OS.ID, m.OS.VersionID), width)
	}

	b.WriteString(strings.Repeat("-", width) + "\n")
	// Steps with durations
	b.WriteString("steps:\n")
	if len(m.Steps) == 0 {
		b.WriteString("  (no steps recorded)\n")
	} else {
		for _, st := range m.Steps {
			line := formatStepLine(st, width-2)
			b.WriteString("  " + line + "\n")
		}
	}

	b.WriteString(strings.Repeat("-", width) + "\n")
	b.WriteString("next actions:\n")
	// Tailscale URL
	b.WriteString(wrapLine("  tailscale: sudo tailscale up", width))
	b.WriteString("\n")
	if tailscaleIP != "" {
		b.WriteString(wrapLine(fmt.Sprintf("  dashboard: http://%s:8484", tailscaleIP), width))
		b.WriteString("\n")
	} else {
		b.WriteString(wrapLine("  dashboard: http://<tailscale-ip>:8484", width))
		b.WriteString("\n")
		b.WriteString(wrapLine("             http://<hostname>.ts.net:8484", width))
		b.WriteString("\n")
	}
	b.WriteString(wrapLine("  health:    omahab doctor", width))
	b.WriteString("\n")
	b.WriteString(wrapLine("  docs:      README.md + DESIGN.md §7", width))
	b.WriteString("\n")

	b.WriteString(headerLine(width))
	return b.String()
}

func headerLine(width int) string {
	if width <= 0 {
		width = 64
	}
	return strings.Repeat("=", width) + "\n"
}

func centerPad(s string, width int) string {
	if len(s) >= width {
		return s[:width]
	}
	pad := width - len(s)
	left := pad / 2
	right := pad - left
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", right)
}

func formatHost(m installer.Manifest) string {
	// Derive host as "<arch> <os pretty>" or arch alone
	var parts []string
	if m.Arch != "" {
		parts = append(parts, m.Arch)
	}
	if m.OS.Pretty != "" {
		parts = append(parts, m.OS.Pretty)
	} else if m.OS.ID != "" {
		parts = append(parts, fmt.Sprintf("%s/%s", m.OS.ID, m.OS.VersionID))
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, " ")
}

func writeKV(b *strings.Builder, key, value string, width int) {
	// "key: value" wrapped to width, key padded to 10
	prefix := fmt.Sprintf("%-10s ", key+":")
	avail := width - len(prefix)
	if avail < 10 {
		avail = 10
	}
	// Wrap value if too long
	wrapped := wrapValue(value, avail)
	lines := strings.Split(wrapped, "\n")
	for i, line := range lines {
		if i == 0 {
			b.WriteString(prefix + line + "\n")
		} else {
			b.WriteString(strings.Repeat(" ", len(prefix)) + line + "\n")
		}
	}
}

func wrapValue(s string, width int) string {
	if len(s) <= width {
		return s
	}
	var out strings.Builder
	for len(s) > width {
		// Prefer break at space
		cut := width
		if idx := strings.LastIndex(s[:width], " "); idx > 0 {
			cut = idx
		}
		out.WriteString(s[:cut])
		out.WriteString("\n")
		s = strings.TrimSpace(s[cut:])
	}
	out.WriteString(s)
	return out.String()
}

func wrapLine(s string, width int) string {
	if len(s) <= width {
		return s
	}
	// Simple word wrap preserving prefix indent
	indent := ""
	if strings.HasPrefix(s, "  ") {
		indent = "  "
		// also preserve second level indent if looks like key: value
	}
	var lines []string
	remaining := s
	first := true
	for len(remaining) > width {
		cut := width
		if idx := strings.LastIndex(remaining[:width], " "); idx > 10 {
			cut = idx
		}
		lines = append(lines, remaining[:cut])
		remaining = strings.TrimSpace(remaining[cut:])
		if first {
			// subsequent lines indented to align
			remaining = indent + "            " + remaining
			first = false
		}
	}
	lines = append(lines, remaining)
	return strings.Join(lines, "\n")
}

func formatStepLine(e installer.JournalEntry, width int) string {
	// Format: "<step padded> <duration> <status>"
	dur := ""
	if e.StartedAt != nil && e.FinishedAt != nil {
		d := e.FinishedAt.Sub(*e.StartedAt).Truncate(time.Second)
		dur = d.String()
	} else if e.Status == installer.JournalCompleted {
		dur = "done"
	}
	statusGlyph := "○"
	switch e.Status {
	case installer.JournalCompleted:
		statusGlyph = "✓"
	case installer.JournalFailed:
		statusGlyph = "✗"
	case installer.JournalRunning:
		statusGlyph = "●"
	case installer.JournalPending:
		statusGlyph = "○"
	}
	// step name truncated/padded to 16, duration 6, glyph 1
	base := fmt.Sprintf("%-16s %6s %s", e.Step, dur, statusGlyph)
	if e.Error != "" {
		// Append error truncated to fit width
		avail := width - len(base) - 3
		if avail > 10 {
			err := e.Error
			if len(err) > avail {
				err = err[:avail-3] + "..."
			}
			base += "  " + err
		}
	}
	if len(base) > width {
		base = base[:width]
	}
	return base
}
