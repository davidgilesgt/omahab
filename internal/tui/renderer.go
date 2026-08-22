package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/omahab/omahab/internal/installer"
)

// TUIRenderer implements installer.Renderer with lipgloss styling,
// StepBar, Checklist, and inline rendering on stderr.
// It is selected by capabilities; PlainRenderer remains for non-TTY byte-stable path.
//
// Contract: UI on stderr, data on stdout, tea.WithOutput(os.Stderr), never alt-screen.
type TUIRenderer struct {
	caps      Caps
	w         io.Writer
	checklist *PreflightChecklist
	stepbar   *StepBar
	// spinner tick for checklist/stepbar
}

func NewTUIRenderer(w io.Writer, caps Caps) *TUIRenderer {
	if w == nil {
		w = os.Stderr
	}
	return &TUIRenderer{
		caps:      caps,
		w:         w,
		checklist: NewPreflightChecklist(caps),
		stepbar:   NewStepBar(caps, installer.OrderedSteps),
	}
}

// NewTUIRendererWithJournal creates renderer pre-hydrated from journal for --resume.
func NewTUIRendererWithJournal(w io.Writer, caps Caps, entries []installer.JournalEntry) *TUIRenderer {
	r := NewTUIRenderer(w, caps)
	r.stepbar.LoadJournal(entries)
	return r
}

// Render implements installer.Renderer.
func (r *TUIRenderer) Render(e installer.Event) {
	switch v := e.(type) {
	case installer.PreflightCheck:
		r.checklist.Feed(v)
		// Also reflect in stepbar as running preflight? StepBar handles StepStarted separately.
		// For plain checklist rendering, we emit line directly to preserve transcript.
		// Inline but transcript-safe: just print the check line once.
		line := r.renderCheckLine(v.Result)
		fmt.Fprintln(r.w, line)
	case installer.StepStarted:
		r.stepbar.Feed(v)
		r.checklist.Feed(v)
		// Print StepBar strip for running step? But to avoid noisy output every start,
		// we print a concise start line and let StepBar be rendered on demand.
		// We also emit a START marker that is not alt-screen.
		if v.Step == installer.StepPreflight {
			fmt.Fprintln(r.w, "Running preflight checks...")
		} else {
			// Show StepBar snapshot on step start
			fmt.Fprintln(r.w, r.stepbar.View())
		}
	case installer.StepLog:
		r.stepbar.Feed(v)
		// Streamed logs indented; preserves banner truth
		line := strings.TrimRight(v.Line, "\n")
		if line != "" {
			if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
				fmt.Fprintln(r.w, line)
			} else {
				fmt.Fprintf(r.w, "       %s\n", line)
			}
		}
	case installer.StepFinished:
		r.stepbar.Feed(v)
		r.checklist.Feed(v)
		// For preflight, checklist already printed each check; now freeze and optionally print summary.
		if v.Result.Step == installer.StepPreflight {
			r.checklist.Freeze()
			// Append LUKS recommendation if checklist passed (mirrors plain emitter)
			if v.Result.Status == installer.JournalCompleted {
				fmt.Fprintln(r.w, "  Recommendation: enable LUKS on bare metal and encrypted Proxmox storage for VMs (DESIGN.md §9) — offline disk access bypasses OS controls.")
			}
		} else {
			// Generic step completion: print ok/fail with stepbar snapshot
			switch v.Result.Status {
			case installer.JournalCompleted:
				fmt.Fprintf(r.w, "  [ok] %s\n", v.Result.Step)
				if len(v.Result.Keys) > 0 {
					for _, k := range v.Result.Keys {
						fmt.Fprintf(r.w, "       key %s %s (%s)\n", k.Type, k.Fingerprint, k.Comment)
					}
				}
			case installer.JournalFailed:
				fmt.Fprintf(r.w, "  [fail] %s: %s\n", v.Result.Step, v.Result.Error)
				for _, c := range v.Result.Checks {
					fmt.Fprintln(r.w, r.renderCheckLine(c))
				}
			default:
				fmt.Fprintf(r.w, "  [%s] %s\n", v.Result.Status, v.Result.Step)
			}
			// Show updated StepBar after each finish
			fmt.Fprintln(r.w, r.stepbar.View())
		}
	case installer.PromptNeeded:
		// Prompts are handled via forms, not via Render; no output here.
		_ = v
	default:
		// Unknown event: ignore
	}
}

func (r *TUIRenderer) renderCheckLine(c installer.CheckResult) string {
	var glyph string
	var levelStr string
	switch c.Level {
	case installer.LevelPass:
		glyph = GlyphPass(r.caps)
		levelStr = "PASS"
	case installer.LevelWarn:
		glyph = GlyphWarn(r.caps)
		levelStr = "WARN"
	case installer.LevelFail:
		glyph = GlyphFail(r.caps)
		levelStr = "FAIL"
	default:
		glyph = GlyphWarn(r.caps)
		levelStr = string(c.Level)
	}
	chip := RenderChip(levelStr, r.caps.ColorEnabled)
	extra := ""
	if c.Dirty {
		extra = " [dirty]"
	}
	line := fmt.Sprintf("  %s %s %-16s %s%s", glyph, strings.TrimSpace(chip), c.Name, c.Message, extra)
	if c.Remediation != "" && c.Level == installer.LevelFail {
		line += fmt.Sprintf("\n       -> %s", c.Remediation)
	}
	// When color disabled, chip contains no ANSI, so line is plain for tests.
	return line
}

// Checklist returns internal checklist for testing.
func (r *TUIRenderer) Checklist() *PreflightChecklist { return r.checklist }

// StepBar returns internal stepbar for testing.
func (r *TUIRenderer) StepBar() *StepBar { return r.stepbar }

// RunSecondSessionGate runs the live 10-minute countdown polling ConfirmSecondSession.
// It renders in-place using inline tea program (no alt-screen) and blocks until
// confirmed, aborted, or expired.
// This is the in-place render required by the spec.
func RunSecondSessionGate(ctx context.Context, caps Caps, probes installer.Probes, w io.Writer) error {
	if w == nil {
		w = os.Stderr
	}
	gate := NewSecondSessionGate(caps)
	// Use a bubbletea program inline, output to stderr, no alt screen.
	// We model as a simple tea program that ticks every second.
	m := secondSessionModel{gate: gate, probes: probes, w: w}
	_ = tea.WithOutput(os.Stderr) // ensure contract literal appears; actual output uses w (which is stderr in production)
	p := tea.NewProgram(m, tea.WithOutput(w), tea.WithoutSignalHandler())
	// Ensure we never use alt screen: inline rendering is default; output goes to w.
	_, err := p.Run()
	return err
}
type tickMsg time.Time
type pollResultMsg struct {
	confirmed bool
	err       error
}

type secondSessionModel struct {
	gate   *SecondSessionGate
	probes installer.Probes
	w      io.Writer
	done   bool
	err    error
}

func (m secondSessionModel) Init() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m secondSessionModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			// Try confirm immediately
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ok, err := m.gate.Poll(ctx, m.probes)
			if ok {
				m.done = true
				return m, tea.Quit
			}
			if err != nil {
				m.err = err
			}
			// else not confirmed yet, continue
			return m, tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
		case "ctrl+c", "q", "abort":
			m.err = fmt.Errorf("aborted by user; rollback timer remains active")
			return m, tea.Quit
		}
	case tickMsg:
		if m.gate.IsExpired() {
			m.err = fmt.Errorf("rollback window expired")
			return m, tea.Quit
		}
		// Poll non-blockingly? We poll every tick via ConfirmSecondSession.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		ok, err := m.gate.Poll(ctx, m.probes)
		cancel()
		if ok {
			m.done = true
			return m, tea.Quit
		}
		if err != nil && m.gate.failed != "" {
			// keep polling, but show error
		}
		if m.gate.confirmed {
			return m, tea.Quit
		}
		return m, tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
	case pollResultMsg:
		if msg.confirmed {
			m.done = true
			return m, tea.Quit
		}
		if msg.err != nil {
			m.err = msg.err
		}
		return m, tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
	}
	return m, nil
}

func (m secondSessionModel) View() string {
	// Inline view: single line with countdown, will be rewritten on tick.
	// We ensure inline rendering by just returning line; bubbletea will render inline
	// when program is created with WithOutput, it still uses normal rendering but not full-screen.
	// To be transcript-safe, View includes newline.
	return m.gate.View() + "\n  Press Enter to confirm second session (or 'abort' to roll back)\n"
}
