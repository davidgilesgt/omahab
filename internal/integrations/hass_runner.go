package integrations

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CommandRunner abstracts hass-cli execution for testability.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

// HassRunnerOptions configures the real HassRunner.
type HassRunnerOptions struct {
	HassCliPath string
	SkillDir    string
	Runner      CommandRunner
}

// RealHassRunner implements HassRunner via direct hass-cli execution and
// Hermes skill file installation.
type RealHassRunner struct {
	hassCliPath string
	skillDir    string
	runner      CommandRunner
}

func NewHassRunner(opts HassRunnerOptions) HassRunner {
	cli := strings.TrimSpace(opts.HassCliPath)
	if cli == "" {
		cli = "hass-cli"
	}
	dir := strings.TrimSpace(opts.SkillDir)
	if dir == "" {
		dir = "/var/lib/omahab/hermes/skills/omahab-home-assistant/SKILL.md"
	}
	r := opts.Runner
	if r == nil {
		r = execCommandRunner{}
	}
	return &RealHassRunner{hassCliPath: cli, skillDir: dir, runner: r}
}

// NewHassRunnerWithDir is a convenience for the integrator to pass a skill
// directory and get a fully-wired runner.
func NewHassRunnerWithDir(skillDir string) HassRunner {
	return NewHassRunner(HassRunnerOptions{SkillDir: skillDir})
}

func (r *RealHassRunner) Validate(ctx context.Context, serverURL, token string) error {
	if strings.TrimSpace(serverURL) == "" {
		return fmt.Errorf("%w: server url required", ErrHAInvalid)
	}
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("%w: token required", ErrHAInvalid)
	}
	// Prefer explicit flags over env to avoid leaking token via process env in tests.
	// hass-cli supports --server and token via HASS_TOKEN env or --token flag depending on version.
	// Try server+token as flags, fallback to env injection via runner that may interpret.
	out, err := r.runner.Run(ctx, r.hassCliPath, "--server", serverURL, "--token", token, "state", "list")
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		// Redact token from message if accidentally echoed.
		if strings.Contains(msg, token) {
			msg = strings.ReplaceAll(msg, token, "[REDACTED]")
		}
		return fmt.Errorf("%w: %s", ErrHAInvalid, msg)
	}
	return nil
}

const hassSkillContent = `---
name: omahab-home-assistant
description: Home Assistant CLI via hass-cli
version: 1.0.0
---
# Home Assistant CLI

Hermes invokes ` + "`hass-cli`" + ` directly; Omahab does not proxy commands. projects must never receive Home Assistant credentials — only the default assistant (` + "`hermes:default`" + `) may use ` + "`HASS_SERVER`" + ` and ` + "`HASS_TOKEN`" + `.

## Configuration

- ` + "`HASS_SERVER`" + ` — Home Assistant URL (e.g. https://home.example.com)
- ` + "`HASS_TOKEN`" + ` — long-lived access token

These are projected via the secrets broker to the default Hermes profile only.

## Validation

On configure, Omahab validates with a read operation:

` + "```" + `
hass-cli --server "$HASS_SERVER" --token "$HASS_TOKEN" state list
` + "```" + `

Any successful read confirms the server URL and token. State-changing calls are not used for validation.

## Usage

Common read operations (approved without extra confirmation):

` + "```" + `
hass-cli state list
hass-cli state get <entity_id>
hass-cli service list
hass-cli --server "$HASS_SERVER" state list
` + "```" + `

State-changing calls require Hermes command approval:

` + "```" + `
hass-cli service call light.turn_on --arguments entity_id=light.living_room
hass-cli service call switch.turn_off --arguments entity_id=switch.kitchen
` + "```" + `

Do not add an MCP server. Use ` + "`hass-cli`" + ` directly.
`

func (r *RealHassRunner) InstallSkill(ctx context.Context) error {
	if strings.TrimSpace(r.skillDir) == "" {
		return fmt.Errorf("skill dir not configured")
	}
	skillPath := strings.TrimSpace(r.skillDir)
	dir := skillPath
	path := skillPath
	if strings.HasSuffix(skillPath, "SKILL.md") {
		dir = filepath.Dir(skillPath)
		path = skillPath
	} else {
		path = filepath.Join(skillPath, "SKILL.md")
		dir = skillPath
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create skill dir: %w", err)
	}
	// Ensure context cancellation is respected
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Write atomically via temp file
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(hassSkillContent), 0o644); err != nil {
		return fmt.Errorf("write skill: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("install skill: %w", err)
	}
	return nil
}

// Ensure interfaces
var _ HassRunner = (*RealHassRunner)(nil)
var _ CommandRunner = execCommandRunner{}
