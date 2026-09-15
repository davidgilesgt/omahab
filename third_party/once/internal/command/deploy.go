package command

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/basecamp/once/internal/docker"
)

type deployCommand struct {
	cmd         *cobra.Command
	flags       settingsFlags
	app         string
	image       string
	hostname    string
	port        int
	healthPath  string
	storagePath string
	proxyBind   string
	tlsMode     string
	secretsFile string
	jsonOutput  bool
}

func newDeployCommand() *deployCommand {
	d := &deployCommand{}
	d.cmd = &cobra.Command{
		Use:   "deploy [<image>]",
		Short: "Deploy an application",
		Args:  cobra.MaximumNArgs(1),
		RunE:  WithNamespace(d.run),
	}

	d.flags.register(d.cmd)
	d.cmd.Flags().StringVar(&d.app, "app", "", "application name (Omahab slug)")
	d.cmd.Flags().StringVar(&d.image, "image", "", "OCI image reference (may include @sha256: digest)")
	d.cmd.Flags().StringVar(&d.hostname, "hostname", "", "hostname for the application")
	d.cmd.Flags().IntVar(&d.port, "port", 80, "container HTTP port (contract: 80)")
	d.cmd.Flags().StringVar(&d.healthPath, "health-path", "/up", "health endpoint (contract: /up)")
	d.cmd.Flags().StringVar(&d.storagePath, "storage", "/storage", "host storage path (contract: /storage)")
	d.cmd.Flags().StringVar(&d.proxyBind, "proxy-bind", "", "loopback bind for kamal-proxy (e.g. 127.0.0.1:8080)")
	d.cmd.Flags().StringVar(&d.tlsMode, "tls", "", "TLS mode: external (Caddy owns TLS) or internal")
	d.cmd.Flags().StringVar(&d.secretsFile, "secrets-file", "", "path to KEY=VAL secrets file (merged into env)")
	d.cmd.Flags().BoolVar(&d.jsonOutput, "json", false, "output JSON")

	return d
}

// deployResult is the single JSON record emitted by --json.
type deployResult struct {
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

func emitJSON(cmd *cobra.Command, v any) error {
	return json.NewEncoder(cmd.OutOrStdout()).Encode(v)
}

func (d *deployCommand) run(ctx context.Context, ns *docker.Namespace, cmd *cobra.Command, args []string) error {
	imageRef := d.image
	if imageRef == "" && len(args) > 0 {
		imageRef = args[0]
	}
	if imageRef == "" {
		return fmt.Errorf("image is required (positional <image> or --image)")
	}

	host := d.hostname
	if host == "" {
		host = d.flags.host
	}
	if host == "" {
		host = docker.NameFromImageRef(imageRef) + ".localhost"
	}

	// Proxy-bind configuration applies before ns.Setup in both deploy and
	// update: the proxy reads it when it boots.
	if err := applyProxyBind(d.proxyBind); err != nil {
		return err
	}

	// Merge --secrets-file into env vars before building settings.
	if d.secretsFile != "" {
		if err := mergeSecretsFile(&d.flags, d.secretsFile); err != nil {
			return err
		}
	}

	// Handle --tls external: force DisableTLS.
	if d.tlsMode == "external" {
		d.flags.disableTLS = true
	}

	if err := ns.Setup(ctx); err != nil {
		return fmt.Errorf("%w: %w", docker.ErrSetupFailed, err)
	}

	if ns.HostInUse(host) {
		return docker.ErrHostnameInUse
	}

	settings, err := d.flags.buildSettings(imageRef, host)
	if err != nil {
		return err
	}

	// Omahab mode: use --app as Name if provided, else generate.
	name := d.app
	if name == "" {
		baseName := docker.NameFromImageRef(imageRef)
		name, err = ns.UniqueName(baseName)
		if err != nil {
			return fmt.Errorf("generating app name: %w", err)
		}
	} else {
		// Ensure app name is unique or use as-is if not in use.
		if existing := ns.Application(name); existing != nil {
			// If app already exists, we will update it rather than fail.
			// For deploy, treat existing app as update.
			existing.Settings = settings
			existing.Settings.Name = name
			existing.Settings.Host = host
			return d.finishDeploy(ctx, cmd, existing, host, name, imageRef)
		}
	}
	settings.Name = name
	settings.Host = host

	app := docker.NewApplication(ns, settings)
	return d.finishDeploy(ctx, cmd, app, host, name, imageRef)
}

// finishDeploy runs the deploy operation and emits exactly one JSON record
// in --json mode (no progress text on stdout). Fresh and existing-app
// deploys share this path.
func (d *deployCommand) finishDeploy(ctx context.Context, cmd *cobra.Command, app *docker.Application, host, name, imageRef string) error {
	var err error
	if d.jsonOutput {
		// No interactive progress wrapper: one JSON record on stdout.
		err = d.runDeployOperation(ctx, app, name, func(docker.DeployProgress) {})
	} else {
		err = d.deployApp(ctx, app, host, name)
	}
	if d.jsonOutput {
		out := deployResult{Status: "ok", Version: imageRef}
		if err != nil {
			out = deployResult{Status: "error", Error: err.Error()}
		}
		if encErr := emitJSON(cmd, out); encErr != nil {
			return encErr
		}
		return err
	}
	return err
}

func (d *deployCommand) deployApp(ctx context.Context, app *docker.Application, host, name string) error {
	return runWithProgress("Deploying "+host, func(progress docker.DeployProgressCallback) error {
		return d.runDeployOperation(ctx, app, name, progress)
	})
}

func (d *deployCommand) runDeployOperation(ctx context.Context, app *docker.Application, name string, progress docker.DeployProgressCallback) error {
	if err := app.Deploy(ctx, progress); err != nil {
		if cleanupErr := app.Destroy(context.Background(), true); cleanupErr != nil {
			slog.Error("Failed to clean up after deploy failure", "app", name, "error", cleanupErr)
		}
		return fmt.Errorf("%w: %w", docker.ErrDeployFailed, err)
	}

	if err := app.VerifyHTTPOrRemove(ctx); err != nil {
		return err
	}

	return nil
}

func parseSecretsFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := make(map[string]string)
	scanner := bufio.NewScanner(f)
	lineno := 0
	for scanner.Scan() {
		lineno++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("invalid line %d in %s: want KEY=VALUE", lineno, path)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// mergeSecretsFile merges a KEY=VAL secrets file into the settings flags env.
// File/env precedence applies; invalid lines fail with path and line only,
// never secret values.
func mergeSecretsFile(f *settingsFlags, path string) error {
	envFromFile, err := parseSecretsFile(path)
	if err != nil {
		return fmt.Errorf("read secrets file: %w", err)
	}
	for k, v := range envFromFile {
		f.env = append(f.env, fmt.Sprintf("%s=%s", k, v))
	}
	return nil
}

// applyProxyBind validates a loopback bind and publishes it for proxy.Boot
// via OMAHAB_PROXY_BIND. Empty keeps the default mode. Malformed binds fail
// instead of silently falling back to port 80.
func applyProxyBind(bind string) error {
	bind = strings.TrimSpace(bind)
	if bind == "" {
		return nil
	}
	host, portStr, err := net.SplitHostPort(bind)
	if err != nil {
		return fmt.Errorf("invalid --proxy-bind %q: want 127.0.0.1:<port> or :<port>", bind)
	}
	if host != "" && host != "127.0.0.1" {
		return fmt.Errorf("invalid --proxy-bind %q: only loopback binds allowed", bind)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("invalid --proxy-bind %q: port must be 1..65535", bind)
	}
	if err := os.Setenv("OMAHAB_PROXY_BIND", bind); err != nil {
		return fmt.Errorf("set proxy bind: %w", err)
	}
	return nil
}
