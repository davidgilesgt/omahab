package workspaces

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/omahab/omahab/internal/domain"
)

// Executor runs external commands so DevPod behavior is testable.
// Production use is ExecExecutor (os/exec); tests supply a fake.
type Executor interface {
	Run(ctx context.Context, name string, args ...string) error
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecExecutor is the production Executor that shells out via os/exec.
// It never injects production secrets or docker socket mounts into the
// workspace container; those values are absent from args and env.
type ExecExecutor struct{}

func (ExecExecutor) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

func (ExecExecutor) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.Output()
}

// DevPodRunnerConfig configures a DevPod-backed Runner. All fields are
// non-secret; the runner never receives production secrets or a docker
// socket mount (enforced by generated config and by argument validation).
type DevPodRunnerConfig struct {
	// Bin is the devpod binary; empty means "devpod" on PATH.
	Bin string
	// Provider is the DevPod provider; empty means "docker".
	Provider string
	// WorkspacesDir is the host base for workspace data; empty means "/srv/omahab/workspaces".
	WorkspacesDir string
	// Executor runs commands; nil means ExecExecutor.
	Executor Executor
	// RepoResolver maps a project ID to its clone URL. If nil, a synthetic
	// placeholder URL is used (useful in tests). The integrator should supply
	// a resolver that reads the projects store so the runner clones the
	// correct project + branch.
	RepoResolver func(context.Context, domain.ID) (string, error)
}

// DevPodRunner is a Runner that shells out to the `devpod` CLI using the
// docker provider. Generated workspace config never mounts the docker socket
// and never receives production secrets. It clones the project at the
// requested branch, applies .devcontainer/devcontainer.json or an Omahab
// default, and installs the selected agent (omp / codex) post-create.
//
// The integrator constructs it with NewDevPodRunner and passes it to
// workspaces.New; docker itself must be installed and the "docker" provider
// available (e.g. `devpod provider add docker`).
type DevPodRunner struct {
	bin           string
	provider      string
	workspacesDir string
	exec          Executor
	resolver      func(context.Context, domain.ID) (string, error)
}

// NewDevPodRunner builds a DevPodRunner from cfg. It applies defaults for
// empty fields and never retains production secrets.
//
// Example for the integrator (internal/controlplane/backend.go:initServices):
//
//	resolver := func(ctx context.Context, id domain.ID) (string, error) {
//	    p, err := projectsService.Get(ctx, id)
//	    if err != nil { return "", err }
//	    return p.RepositoryURL, nil
//	}
//	runner := workspaces.NewDevPodRunner(workspaces.DevPodRunnerConfig{
//	    WorkspacesDir: cfg.DataDir + "/workspaces",
//	    RepoResolver:  resolver,
//	})
//	svc := workspaces.New(db, runner)
func NewDevPodRunner(cfg DevPodRunnerConfig) *DevPodRunner {
	bin := strings.TrimSpace(cfg.Bin)
	if bin == "" {
		bin = "devpod"
	}
	provider := strings.TrimSpace(cfg.Provider)
	if provider == "" {
		provider = "docker"
	}
	dir := strings.TrimSpace(cfg.WorkspacesDir)
	if dir == "" {
		dir = "/srv/omahab/workspaces"
	}
	ex := cfg.Executor
	if ex == nil {
		ex = ExecExecutor{}
	}
	return &DevPodRunner{
		bin:           bin,
		provider:      provider,
		workspacesDir: dir,
		exec:          ex,
		resolver:      cfg.RepoResolver,
	}
}

func (r *DevPodRunner) resolveRepo(ctx context.Context, projectID domain.ID) string {
	if r.resolver != nil {
		if u, err := r.resolver(ctx, projectID); err == nil && strings.TrimSpace(u) != "" {
			return strings.TrimSpace(u)
		}
	}
	// placeholder — safe for tests and preserves command shape without leaking secrets
	return "https://forgejo.local/" + string(projectID) + ".git"
}

func (r *DevPodRunner) defaultDevcontainerJSON(agent string) string {
	// Omahab default devcontainer: no docker socket mount, no secrets.
	// postCreateCommand installs the selected agent inside the container.
	postCreate := ""
	switch agent {
	case "omp":
		postCreate = "curl -fsSL https://omahab.local/install-omp.sh | bash || true"
	case "codex":
		postCreate = "npm install -g @openai/codex || true"
	default:
		postCreate = ""
	}
	// Deliberately no "mounts": [ ... docker.sock ... ] and no secrets.
	if postCreate != "" {
		return fmt.Sprintf(`{"name":"omahab-workspace","image":"mcr.microsoft.com/devcontainers/base:ubuntu","postCreateCommand":%q}`, postCreate)
	}
	return `{"name":"omahab-workspace","image":"mcr.microsoft.com/devcontainers/base:ubuntu"}`
}

// Up creates a workspace container via `devpod up` with the docker provider.
// It clones project@branch, applies the devcontainer or the Omahab default,
// and installs the selected agent (omp / codex) post-create. The generated
// config never mounts the docker socket and never receives production secrets.
func (r *DevPodRunner) Up(ctx context.Context, workspaceID string, projectID domain.ID, branch, agent string, opts RunnerOpts) error {
	if strings.TrimSpace(workspaceID) == "" {
		return fmt.Errorf("%w: workspace id is required", ErrValidation)
	}
	if branch == "" {
		branch = "main"
	}
	// Validate agent already done by Service, but enforce allowlist here too.
	if agent != "" && !allowedAgents[agent] {
		return fmt.Errorf("%w: unsupported agent %q", ErrValidation, agent)
	}
	repoURL := r.resolveRepo(ctx, projectID)

	wsDir := filepath.Join(r.workspacesDir, workspaceID)
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		return fmt.Errorf("create workspace dir: %w", err)
	}

	source := strings.TrimSpace(repoURL)
	if source == "" {
		return fmt.Errorf("%w: repository url is required", ErrValidation)
	}
	// devpod source with branch: repo@branch
	if branch != "" {
		// Avoid duplicating @ if resolver already contains it.
		if !strings.Contains(source, "@") {
			source = source + "@" + branch
		}
	}

	var devcontainerPath string
	if opts.DevcontainerSource == "" || opts.DevcontainerSource == "default" {
		devcontainerPath = filepath.Join(wsDir, "devcontainer.json")
		content := r.defaultDevcontainerJSON(agent)
		if strings.Contains(content, "docker.sock") {
			return fmt.Errorf("generated devcontainer must not mount docker socket")
		}
		if err := os.WriteFile(devcontainerPath, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write devcontainer: %w", err)
		}
	} else {
		// use project's .devcontainer/devcontainer.json
		devcontainerPath = ".devcontainer/devcontainer.json"
	}

	args := []string{"up", workspaceID, "--provider", r.provider, "--source", source, "--ide", "none"}
	if devcontainerPath != "" {
		// Only pass path when we generated a default; for "devcontainer" the
		// project file is discovered inside the cloned source, but passing
		// the path is still explicit and testable.
		args = append(args, "--devcontainer-path", devcontainerPath)
	}
	// Guard: must not contain secrets or docker socket mounts.
	for _, a := range args {
		low := strings.ToLower(a)
		if strings.Contains(low, "docker.sock") {
			return fmt.Errorf("devpod args must not mount docker socket")
		}
		if strings.Contains(low, "secret") {
			return fmt.Errorf("devpod args must not contain secrets")
		}
	}
	if err := r.exec.Run(ctx, r.bin, args...); err != nil {
		return fmt.Errorf("devpod up: %w", err)
	}
	if agent != "" {
		if err := r.installAgent(ctx, workspaceID, agent); err != nil {
			return err
		}
	}
	return nil
}

func (r *DevPodRunner) installAgent(ctx context.Context, workspaceID, agent string) error {
	var installCmd string
	switch agent {
	case "omp":
		installCmd = "curl -fsSL https://omahab.local/install-omp.sh | bash"
	case "codex":
		installCmd = "npm install -g @openai/codex"
	default:
		installCmd = "echo installing " + agent
	}
	args := []string{"ssh", workspaceID, "--command", installCmd}
	return r.exec.Run(ctx, r.bin, args...)
}

// Stop stops a workspace via `devpod stop <id>`.
func (r *DevPodRunner) Stop(ctx context.Context, workspaceID string) error {
	if strings.TrimSpace(workspaceID) == "" {
		return fmt.Errorf("%w: workspace id is required", ErrValidation)
	}
	args := []string{"stop", workspaceID}
	return r.exec.Run(ctx, r.bin, args...)
}

// Delete removes a workspace via `devpod delete <id> --force`.
func (r *DevPodRunner) Delete(ctx context.Context, workspaceID string) error {
	if strings.TrimSpace(workspaceID) == "" {
		return fmt.Errorf("%w: workspace id is required", ErrValidation)
	}
	args := []string{"delete", workspaceID, "--force"}
	return r.exec.Run(ctx, r.bin, args...)
}

// Attach creates-or-attaches a resumable tmux session named per workspace
// running `devpod ssh --tty`. The session name is deterministic
// (omahab-<workspaceID>) so repeated attaches rejoin the same shell.
func (r *DevPodRunner) Attach(ctx context.Context, workspaceID string) error {
	if strings.TrimSpace(workspaceID) == "" {
		return fmt.Errorf("%w: workspace id is required", ErrValidation)
	}
	session := r.sessionName(workspaceID)
	// tmux new-session -A -s <session> devpod ssh <id> --tty
	// -A create-or-attach, -s session name, remaining args become the command.
	args := []string{"new-session", "-A", "-s", session, r.bin, "ssh", workspaceID, "--tty"}
	return r.exec.Run(ctx, "tmux", args...)
}

func (r *DevPodRunner) sessionName(workspaceID string) string {
	name := "omahab-" + workspaceID
	if len(name) > 64 {
		name = name[:64]
	}
	name = strings.ReplaceAll(name, ".", "-")
	name = strings.ReplaceAll(name, ":", "-")
	name = strings.ReplaceAll(name, "/", "-")
	return name
}

// IsRunning reports whether the workspace is running via `devpod status --output json`.
func (r *DevPodRunner) IsRunning(ctx context.Context, workspaceID string) (bool, error) {
	if strings.TrimSpace(workspaceID) == "" {
		return false, fmt.Errorf("%w: workspace id is required", ErrValidation)
	}
	out, err := r.exec.Output(ctx, r.bin, "status", workspaceID, "--output", "json")
	if err != nil {
		// devpod returns non-zero when workspace not found / not running.
		return false, nil
	}
	low := strings.ToLower(string(out))
	if strings.Contains(low, "\"status\"") {
		return strings.Contains(low, "running"), nil
	}
	return strings.Contains(low, "running"), nil
}

// SessionNameForTest exposes session naming for tests without exporting internals broadly.
func (r *DevPodRunner) SessionNameForTest(workspaceID string) string {
	return r.sessionName(workspaceID)
}
