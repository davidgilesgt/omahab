package installer

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// PromptKind identifies a user prompt.
type PromptKind string

const (
	PromptKindSSHKeys      PromptKind = "ssh_keys"
	PromptKindRecoveryKey  PromptKind = "recovery_key"
	PromptKindApexDomain   PromptKind = "apex_domain"
	PromptKindTokenA       PromptKind = "token_a"
	PromptKindTokenB       PromptKind = "token_b"
	PromptKindTailscale    PromptKind = "tailscale"
	PromptKindGitHubUser   PromptKind = "github_user"
	PromptKindKeyFile      PromptKind = "key_file"
)

// Event is a typed installer event. The typed structs below implement this
// interface. Use a type switch to handle each kind.
type Event interface {
	isEvent()
}

// PreflightCheck is emitted once per CheckResult during preflight.
type PreflightCheck struct {
	Result CheckResult `json:"result"`
}

func (PreflightCheck) isEvent() {}

// StepStarted is emitted when a step begins.
type StepStarted struct {
	Step string `json:"step"`
}

func (StepStarted) isEvent() {}

// StepLog is emitted for incremental progress during a long step.
// The contract requires that packages and daemon health poll emit
// StepLog lines so the "progress is streamed per step" banner is true.
type StepLog struct {
	Step string `json:"step"`
	Line string `json:"line"`
}

func (StepLog) isEvent() {}

// StepFinished is emitted when a step completes.
type StepFinished struct {
	Result RunResult `json:"result"`
}

func (StepFinished) isEvent() {}

// PromptNeeded is emitted when interactive input is required.
type PromptNeeded struct {
	Kind PromptKind `json:"kind"`
}

func (PromptNeeded) isEvent() {}

// EventTypeFor returns the canonical type string for an Event, used for JSON
// encoding. It mirrors the JSON "type" field.
func EventTypeFor(e Event) string {
	switch e.(type) {
	case PreflightCheck:
		return "preflight_check"
	case StepStarted:
		return "step_started"
	case StepLog:
		return "step_log"
	case StepFinished:
		return "step_finished"
	case PromptNeeded:
		return "prompt_needed"
	default:
		return "unknown"
	}
}

// MarshalEvent encodes an Event as a single JSON object with a "type"
// discriminator. The output is one JSON object per event, suitable for
// --json streaming (one object per line).
func MarshalEvent(e Event) ([]byte, error) {
	switch v := e.(type) {
	case PreflightCheck:
		return json.Marshal(map[string]any{
			"type":   "preflight_check",
			"result": v.Result,
		})
	case StepStarted:
		return json.Marshal(map[string]any{
			"type": "step_started",
			"step": v.Step,
		})
	case StepLog:
		return json.Marshal(map[string]any{
			"type": "step_log",
			"step": v.Step,
			"line": v.Line,
		})
	case StepFinished:
		return json.Marshal(map[string]any{
			"type":   "step_finished",
			"result": v.Result,
		})
	case PromptNeeded:
		return json.Marshal(map[string]any{
			"type": "prompt_needed",
			"kind": v.Kind,
		})
	default:
		return nil, fmt.Errorf("unknown event type %T", e)
	}
}

// NewJSONEmitter returns an Emit func that writes one JSON object per event
// to w, each followed by a newline. The writer is typically os.Stderr when
// UI is on stderr, or os.Stdout when JSON is the data channel. Callers must
// choose the channel that matches "UI on stderr, data on stdout" per DESIGN.md
// §5.3 when wiring the emitter.
func NewJSONEmitter(w io.Writer) func(Event) {
	return func(e Event) {
		data, err := MarshalEvent(e)
		if err != nil {
			// fallback: emit an error envelope rather than dropping
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": err.Error()})
			return
		}
		_, _ = w.Write(data)
		_, _ = w.Write([]byte("\n"))
	}
}

// NewPlainEmitter returns an Emit func that reproduces today's printf
// transcript byte-for-byte when fed the same event stream that the old
// printf-based code would have printed. It writes to w with optional ANSI
// color. The rendering is intentionally narrow and stateless; a later TUI
// agent swaps this implementation for a Bubble Tea renderer without changing
// the Service's event emission.
func NewPlainEmitter(w io.Writer, useColor bool) func(Event) {
	return func(e Event) {
		switch v := e.(type) {
		case PreflightCheck:
			c := v.Result
			var status string
			switch c.Level {
			case LevelPass:
				if useColor {
					status = "\033[32mPASS\033[0m"
				} else {
					status = "PASS"
				}
			case LevelWarn:
				if useColor {
					status = "\033[33mWARN\033[0m"
				} else {
					status = "WARN"
				}
			case LevelFail:
				if useColor {
					status = "\033[31mFAIL\033[0m"
				} else {
					status = "FAIL"
				}
			default:
				status = string(c.Level)
			}
			extra := ""
			if c.Dirty {
				extra = " [dirty]"
			}
			fmt.Fprintf(w, "  %s %-16s %s%s\n", status, c.Name, c.Message, extra)
			if c.Remediation != "" && c.Level == LevelFail {
				fmt.Fprintf(w, "       -> %s\n", c.Remediation)
			}
			// LUKS recommendation is not emitted per-check; it is emitted once
			// after the full preflight set via the service's StepFinished
			// handling or via the CLI's direct print. To keep NewPlainEmitter
			// byte-for-byte for per-check events, we do not emit it here.
			// The CLI's preflight rendering appends it after the last check.
		case StepStarted:
			// The original printf loop only printed "[ok]/[fail]" after the step
			// finished, not at start. Emitting a start line would diverge from
			// the captured transcript. Therefore StepStarted is a no-op for the
			// plain transcript, but it enables the TUI's StepBar spinner.
			_ = v
		case StepLog:
			// Streamed progress lines: indented under the step, matching the
			// style of key lines ("       key ...") but without coupling to a
			// specific format. For byte stability, we print "       %s\n" for
			// generic logs and keep the original key-line format elsewhere.
			// Packages and daemon health poll use this to make the banner true.
			line := strings.TrimRight(v.Line, "\n")
			if line != "" {
				// If line already contains indentation, preserve it; otherwise indent.
				if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
					fmt.Fprintf(w, "%s\n", line)
				} else {
					fmt.Fprintf(w, "       %s\n", line)
				}
			}
		case StepFinished:
			r := v.Result
			switch r.Status {
			case JournalCompleted:
				fmt.Fprintf(w, "  [ok] %s\n", r.Step)
				if r.Step == StepPreflight && len(r.Checks) > 0 {
					// already printed via PreflightCheck events; do not duplicate
				}
				if len(r.Keys) > 0 {
					for _, k := range r.Keys {
						fmt.Fprintf(w, "       key %s %s (%s)\n", k.Type, k.Fingerprint, k.Comment)
					}
				}
				// LUKS recommendation: append once after preflight step completes
				// to satisfy the acceptance that preflight output contains the
				// encrypted-storage guidance.
				if r.Step == StepPreflight {
					fmt.Fprintln(w, "  Recommendation: enable LUKS on bare metal and encrypted Proxmox storage for VMs (DESIGN.md §9) — offline disk access bypasses OS controls.")
				}
			case JournalFailed:
				fmt.Fprintf(w, "  [fail] %s: %s\n", r.Step, r.Error)
				if len(r.Checks) > 0 {
					for _, c := range r.Checks {
						var status string
						switch c.Level {
						case LevelPass:
							if useColor {
								status = "\033[32mPASS\033[0m"
							} else {
								status = "PASS"
							}
						case LevelWarn:
							if useColor {
								status = "\033[33mWARN\033[0m"
							} else {
								status = "WARN"
							}
						case LevelFail:
							if useColor {
								status = "\033[31mFAIL\033[0m"
							} else {
								status = "FAIL"
							}
						default:
							status = string(c.Level)
						}
						extra := ""
						if c.Dirty {
							extra = " [dirty]"
						}
						fmt.Fprintf(w, "  %s %-16s %s%s\n", status, c.Name, c.Message, extra)
						if c.Remediation != "" && c.Level == LevelFail {
							fmt.Fprintf(w, "       -> %s\n", c.Remediation)
						}
					}
				}
			default:
				// For other statuses (e.g., pending/running) we mirror the old
				// journal snapshot format: "  [done] step" / "  [failed] step: err"
				// but those are not emitted via StepFinished in the old code.
				fmt.Fprintf(w, "  [%s] %s\n", r.Status, r.Step)
			}
		case PromptNeeded:
			// Plain transcript does not render PromptNeeded; orchestration prints
			// the prompt via the Renderer. No output here to keep byte stability.
			_ = v
		}
	}
}

// Emit is a helper that calls opts.Emit if non-nil.
func EmitEvent(emit func(Event), e Event) {
	if emit != nil {
		emit(e)
	}
}

// Renderer is the narrow rendering interface that the CLI implements.
// The Service emits Events via Options.Emit; the CLI's Renderer translates
// those Events to terminal output. The later TUI agent swaps the
// implementation without touching Service logic. This interface is intentionally
// small: one method per event kind is enough, but we expose a single Render
// method to keep the seam minimal.
type Renderer interface {
	Render(e Event)
}

// PlainRenderer implements Renderer by delegating to NewPlainEmitter. It exists
// so the integrator can construct a renderer with the capability ladder
// (isTTY, colorProfile, width) and pass it to the CLI. The Service itself
// never depends on a Renderer; it only calls Options.Emit.
type PlainRenderer struct {
	emit func(Event)
}

// NewPlainRenderer constructs a Renderer that writes UI to w with optional
// color. The TUI agent will provide an alternative implementation of
// Renderer with the same method set.
func NewPlainRenderer(w io.Writer, useColor bool) *PlainRenderer {
	return &PlainRenderer{emit: NewPlainEmitter(w, useColor)}
}

// Render implements Renderer.
func (r *PlainRenderer) Render(e Event) {
	if r.emit != nil {
		r.emit(e)
	}
}

// Capabilities describes the terminal capability ladder resolved once at
// startup per DESIGN.md §5.3.
type Capabilities struct {
	IsTTY        bool
	ColorEnabled bool
	Width        int
}

// ResolveCapabilities computes Capabilities from the process environment and
// flags. It is the single place where NO_COLOR, --no-color, TERM=dumb, and
// isTTY are considered. The caller passes the resolved isTTY for the UI
// stream (stderr), the explicit --no-color flag, and the color choice.
// Width defaults to 80 when unknown.
func ResolveCapabilities(isTTY bool, noColorFlag bool, term string, noColorEnv string) Capabilities {
	colorEnabled := true
	if noColorEnv != "" || noColorFlag || term == "dumb" {
		colorEnabled = false
	}
	if !isTTY {
		// Non-TTY downgrades color but does not affect content width fallback.
		// We keep colorEnabled false when not a TTY to avoid ANSI leaks into pipes.
		colorEnabled = false
	}
	width := 80
	// Width detection is optional; we keep a safe default for the receipt
	// (≤64 columns). Callers may override via COLUMNS or terminal query.
	return Capabilities{IsTTY: isTTY, ColorEnabled: colorEnabled, Width: width}
}
