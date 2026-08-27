package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/omahab/omahab/internal/installer"
)

// StepState represents the visual state of a step in the StepBar.
type StepState string

const (
	StepPending   StepState = "pending"
	StepRunning   StepState = "running"
	StepCompleted StepState = "completed"
	StepFailed    StepState = "failed"
)

// StepBar renders journal steps inline: `ssh_keys ✓ · sshd ● 14s · packages ○`
// It is bound to OrderedSteps and updates via events and journal snapshots.
// --resume pre-checks from journal state, then follows live Stream.
type StepBar struct {
	caps        Caps
	steps       []string
	state       map[string]StepState
	startedAt   map[string]time.Time
	finishedAt  map[string]time.Time
	elapsed     map[string]time.Duration
	runningStep string
	now         func() time.Time
}

// NewStepBar creates a StepBar with initial steps.
// caps controls glyph fallback.
func NewStepBar(caps Caps, steps []string) *StepBar {
	if steps == nil {
		steps = installer.OrderedSteps
	}
	m := &StepBar{
		caps:       caps,
		steps:      steps,
		state:      make(map[string]StepState),
		startedAt:  make(map[string]time.Time),
		finishedAt: make(map[string]time.Time),
		elapsed:    make(map[string]time.Duration),
		now:        time.Now,
	}
	for _, s := range steps {
		m.state[s] = StepPending
	}
	return m
}

// SetNow overrides clock for tests.
func (s *StepBar) SetNow(fn func() time.Time) { s.now = fn }

// LoadJournal hydrates bar from journal entries (for --resume pre-check).
// Completed steps are marked ✓, failed as ✗, running as ●, pending as ○.
func (s *StepBar) LoadJournal(entries []installer.JournalEntry) {
	for _, e := range entries {
		switch e.Status {
		case installer.JournalCompleted:
			s.state[e.Step] = StepCompleted
			if e.StartedAt != nil && e.FinishedAt != nil {
				s.elapsed[e.Step] = e.FinishedAt.Sub(*e.StartedAt)
				s.startedAt[e.Step] = *e.StartedAt
				s.finishedAt[e.Step] = *e.FinishedAt
			}
		case installer.JournalFailed:
			s.state[e.Step] = StepFailed
		case installer.JournalRunning:
			s.state[e.Step] = StepRunning
			s.runningStep = e.Step
			if e.StartedAt != nil {
				s.startedAt[e.Step] = *e.StartedAt
			}
		case installer.JournalPending:
			s.state[e.Step] = StepPending
		}
	}
}

// Feed ingests an installer event.
func (s *StepBar) Feed(e installer.Event) {
	switch v := e.(type) {
	case installer.StepStarted:
		s.state[v.Step] = StepRunning
		s.runningStep = v.Step
		if _, ok := s.startedAt[v.Step]; !ok {
			s.startedAt[v.Step] = s.now()
		}
	case installer.StepLog:
		// StepLog indicates running; ensure state is running
		if st, ok := s.state[v.Step]; !ok || st != StepRunning {
			s.state[v.Step] = StepRunning
			s.runningStep = v.Step
			if _, ok := s.startedAt[v.Step]; !ok {
				s.startedAt[v.Step] = s.now()
			}
		}
	case installer.StepFinished:
		res := v.Result
		switch res.Status {
		case installer.JournalCompleted:
			s.state[res.Step] = StepCompleted
		case installer.JournalFailed:
			s.state[res.Step] = StepFailed
		default:
			s.state[res.Step] = StepCompleted
		}
		if res.Step == s.runningStep {
			s.runningStep = ""
		}
		if st, ok := s.startedAt[res.Step]; ok {
			s.elapsed[res.Step] = s.now().Sub(st)
			now := s.now()
			s.finishedAt[res.Step] = now
		}
	}
}

// View renders the StepBar strip.
// For interactive TTY it is width-safe (compact) to keep a single pinned line;
// for plain/log output it returns the full unwrapped strip.
func (s *StepBar) View() string {
	parts := s.buildParts()
	full := strings.Join(parts, " "+GlyphDot()+" ")
	if !s.caps.IsTTY || !s.caps.ColorEnabled {
		return full
	}
	width := s.caps.Width
	if width <= 0 {
		width = 80
	}
	if lipgloss.Width(full) <= width {
		return full
	}
	return s.compactView(parts, width)
}

func (s *StepBar) buildParts() []string {
	parts := make([]string, 0, len(s.steps))
	for _, step := range s.steps {
		st := s.state[step]
		var glyph string
		var suffix string
		switch st {
		case StepCompleted:
			glyph = GlyphStepDone(s.caps)
			if d, ok := s.elapsed[step]; ok && d > 0 {
				suffix = fmt.Sprintf(" %s", formatDuration(d))
			}
		case StepRunning:
			glyph = GlyphStepRunning(s.caps)
			if t, ok := s.startedAt[step]; ok {
				d := s.now().Sub(t).Truncate(time.Second)
				suffix = fmt.Sprintf(" %s", formatDuration(d))
			} else {
				suffix = " ..."
			}
		case StepFailed:
			glyph = GlyphFail(s.caps)
		case StepPending:
			glyph = GlyphStepPending(s.caps)
		default:
			glyph = GlyphStepPending(s.caps)
		}
		part := fmt.Sprintf("%s %s%s", step, glyph, suffix)
		parts = append(parts, part)
	}
	return parts
}

func (s *StepBar) compactView(parts []string, width int) string {
	// Find running index.
	runningIdx := -1
	for i, step := range s.steps {
		if step == s.runningStep {
			runningIdx = i
			break
		}
	}
	if runningIdx == -1 {
		for i, step := range s.steps {
			if s.state[step] == StepPending {
				runningIdx = i
				break
			}
		}
		if runningIdx == -1 {
			runningIdx = len(s.steps) / 2
		}
	}
	lo := runningIdx - 1
	if lo < 0 {
		lo = 0
	}
	hi := runningIdx + 1
	if hi >= len(parts) {
		hi = len(parts) - 1
	}
	var compactParts []string
	if lo > 0 {
		compactParts = append(compactParts, parts[0])
		if lo == 2 {
			compactParts = append(compactParts, parts[1])
		} else if lo > 2 {
			compactParts = append(compactParts, "…")
		}
	}
	for i := lo; i <= hi; i++ {
		compactParts = append(compactParts, parts[i])
	}
	if hi < len(parts)-1 {
		if hi == len(parts)-3 {
			compactParts = append(compactParts, parts[len(parts)-2])
		} else if hi < len(parts)-3 {
			compactParts = append(compactParts, "…")
		}
		compactParts = append(compactParts, parts[len(parts)-1])
	}
	candidate := strings.Join(compactParts, " "+GlyphDot()+" ")
	if lipgloss.Width(candidate) <= width {
		return candidate
	}
	completed := 0
	for _, st := range s.state {
		if st == StepCompleted {
			completed++
		}
	}
	runningPart := parts[runningIdx]
	summary := fmt.Sprintf("%d/%d", completed, len(s.steps))
	minimal := fmt.Sprintf("%s %s %s", runningPart, GlyphDot(), summary)
	if lipgloss.Width(minimal) <= width {
		return minimal
	}
	return runningPart
}

func formatDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return d.String()
}

// CompactStepBarForStatus renders a one-line status strip for `omahab status`
// reusing StepBar glyphs. It maps a single health status to a StepBar entry.
func CompactStatusStrip(health string, caps Caps) string {
	sb := NewStepBar(caps, []string{"status"})
	// Map health to state
	switch strings.ToLower(health) {
	case "healthy", "ok", "pass":
		sb.state["status"] = StepCompleted
	case "degraded", "warn":
		sb.state["status"] = StepRunning
	case "unhealthy", "fail", "error":
		sb.state["status"] = StepFailed
	default:
		sb.state["status"] = StepPending
	}
	return sb.View()
}
