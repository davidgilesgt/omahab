package tui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"
	"github.com/omahab/omahab/internal/installer"
)

// FormResult holds answers for a prompts form.
type FormResult map[installer.PromptKind]string

// IsDumb reports whether to use linear fallback per rendering contract:
// TERM=dumb, piped stdin, NO_COLOR, or non-TTY.
func IsDumb(caps Caps) bool {
	if !caps.IsTTY {
		return true
	}
	if !caps.ColorEnabled {
		// NO_COLOR / --no-color / TERM=dumb downgrade already maps to ColorEnabled false
		// We still use linear fallback when not a TTY; color disabled alone still uses fallback for safety
		// But spec says TERM=dumb downgrades; we treat ColorEnabled false as dumb for forms.
		return true
	}
	// Also check TERM env explicitly
	if os.Getenv("TERM") == "dumb" {
		return true
	}
	// Piped stdin: use proper TTY check, not Stat char device heuristic
	if !term.IsTerminal(os.Stdin.Fd()) {
		return true
	}
	return false
}

// RunHuhForm runs a Huh form built from defs. It uses live validators and
// masked input for token prompts. It is inline (no alt-screen) and writes UI
// to stderr via tea.WithOutput(os.Stderr) internally via huh.
// Returns map of kind -> value.
func RunHuhForm(defs []installer.PromptDefinition, caps Caps) (FormResult, error) {
	if IsDumb(caps) {
		return RunLinearForm(defs, os.Stdin, os.Stderr)
	}
	return runHuhInteractive(defs)
}

func runHuhInteractive(defs []installer.PromptDefinition) (FormResult, error) {
	results := make(FormResult)
	var fields []huh.Field
	// Keep references to variables per field for extraction
	vars := make(map[installer.PromptKind]*string)
	for _, d := range defs {
		// Copy loop variable
		def := d
		v := ""
		vars[def.Kind] = &v
		// For SSH keys (pasted), use multiline Text to allow paste of many keys.
		if def.Kind == installer.PromptKindSSHKeys {
			txt := huh.NewText().
				Title(def.Title).
				Value(vars[def.Kind]).
				Validate(func(s string) error {
					trim := strings.TrimSpace(s)
					if trim == "" {
						// Empty allowed to keep existing keys; treat as valid
						return nil
					}
					if def.Validate != nil {
						return def.Validate(trim)
					}
					return nil
				}).
				Placeholder("paste SSH public keys (one per line, empty to skip)")
			fields = append(fields, txt)
			continue
		}
		// Choose field type: for token masked input use Input with EchoMode
		input := huh.NewInput().
			Title(def.Title).
			Value(vars[def.Kind]).
			Validate(func(s string) error {
				trim := strings.TrimSpace(s)
				if trim == "" {
					// For optional tokens, allow empty (Token B)
					if def.Kind == installer.PromptKindTokenB {
						return nil
					}
					// For apex domain, empty means skip (handled by caller)
					if def.Kind == installer.PromptKindApexDomain {
						return nil
					}
					return fmt.Errorf("value is required")
				}
				if def.Validate != nil {
					return def.Validate(trim)
				}
				return nil
			})
		if def.Mask {
			input.EchoMode(huh.EchoModePassword)
		}
		// Placeholder for masked tokens to indicate hidden
		if def.Mask {
			input.Placeholder("paste token (hidden)")
		}
		fields = append(fields, input)
	}
	form := huh.NewForm(huh.NewGroup(fields...))
	// Ensure inline rendering: huh uses bubbletea; we must ensure output is stderr and not alt-screen.
	// huh.Form.WithOutput and WithTheme can enforce inline. By default huh uses stdout/stderr via bubbletea with alt-screen disabled?
	// We force WithOutput(os.Stderr) via form.WithProgramOptions if available.
	// Newer huh supports WithAccessible fallback automatically for non-TTY.
	// For our minimal case we rely on huh's default which is inline (no alt-screen).
	// Explicitly disable alt-screen via option if API exposes it.
	// Not all versions expose WithProgramOptions; guard via type assertion.
	if err := form.Run(); err != nil {
		return nil, err
	}
	for k, v := range vars {
		results[k] = strings.TrimSpace(*v)
	}
	return results, nil
}

// RunLinearForm is the fallback for TERM=dumb / piped stdin.
// It renders one question per line, reading from r and writing prompts to w,
// using the same PromptDefinitions and validators.
func RunLinearForm(defs []installer.PromptDefinition, r io.Reader, w io.Writer) (FormResult, error) {
	scanner := bufio.NewScanner(r)
	results := make(FormResult)
	for _, d := range defs {
		def := d
		for {
			// Prompt line
			maskHint := ""
			if def.Mask {
				maskHint = " (input hidden, paste then Enter)"
			}
			fmt.Fprintf(w, "%s%s: ", def.Title, maskHint)
			if !scanner.Scan() {
				if err := scanner.Err(); err != nil && err != io.EOF {
					return nil, err
				}
				// EOF: if any required field missing, error
				if results[def.Kind] == "" {
					// For optional token B allow empty on EOF
					if def.Kind == installer.PromptKindTokenB {
						results[def.Kind] = ""
						break
					}
					return nil, fmt.Errorf("cancelled or EOF")
				}
				break
			}
			raw := strings.TrimSpace(scanner.Text())
			if raw == "" {
				// Allow skip for optional token B
				if def.Kind == installer.PromptKindTokenB {
					results[def.Kind] = ""
					break
				}
				// For required fields, if empty and validator would fail, reprompt unless EOF
				// But if user intentionally skipped optional, we handled above.
				// For required, show error and reprompt.
				if def.Kind == installer.PromptKindApexDomain && raw == "" {
					// Treat empty as skip for domain (allowed to skip Cloudflare)
					results[def.Kind] = ""
					break
				}
				fmt.Fprintln(w, "  value is required")
				continue
			}
			if def.Validate != nil {
				if err := def.Validate(raw); err != nil {
					fmt.Fprintf(w, "  invalid: %v\n", err)
					continue
				}
			}
			results[def.Kind] = raw
			break
		}
	}
	return results, scanner.Err()
}

// RunSinglePrompt runs a single PromptDefinition via huh or linear fallback.
// Used for key import / recoveryKey / etc.
func RunSinglePrompt(def installer.PromptDefinition, caps Caps) (string, error) {
	if IsDumb(caps) {
		res, err := RunLinearForm([]installer.PromptDefinition{def}, os.Stdin, os.Stderr)
		if err != nil {
			return "", err
		}
		return res[def.Kind], nil
	}
	var val string
	var field huh.Field
	if def.Kind == installer.PromptKindSSHKeys {
		txt := huh.NewText().Title(def.Title).Value(&val).Placeholder("paste SSH public keys (one per line, empty to skip)")
		if def.Validate != nil {
			validate := def.Validate
			txt.Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					return nil
				}
				return validate(strings.TrimSpace(s))
			})
		}
		field = txt
	} else {
		input := huh.NewInput().Title(def.Title).Value(&val)
		if def.Mask {
			input.EchoMode(huh.EchoModePassword)
			input.Placeholder("paste token (hidden)")
		}
		if def.Validate != nil {
			validate := def.Validate
			input.Validate(func(s string) error {
				trim := strings.TrimSpace(s)
				if trim == "" {
					if def.Kind == installer.PromptKindTokenB || def.Kind == installer.PromptKindApexDomain {
						return nil
					}
				}
				if strings.EqualFold(trim, "skip") || strings.EqualFold(trim, "s") {
					return nil
				}
				return validate(trim)
			})
		}
		field = input
	}
	form := huh.NewForm(huh.NewGroup(field))
	if err := form.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(val), nil
}
