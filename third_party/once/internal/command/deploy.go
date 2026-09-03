package command

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
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

// Private

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

	// Handle --proxy-bind for loopback proxy: store for proxy Boot.
	// The proxy Boot will use this to publish 127.0.0.1:<port>:80 only.
	if d.proxyBind != "" {
		// Parse host:port, extract port for proxy settings.
		// We stash the bind in the namespace via a label or direct proxy settings.
		// For now, we set an env to signal proxy to use loopback.
		// The proxy.Boot will check for this via context or flag.
		// Simplest: set env var for this process that proxy will read?
		// Instead, we directly configure proxy settings before Setup.
		if err := applyProxyBind(ns, d.proxyBind); err != nil {
			return err
		}
	}

	// Merge --secrets-file into env vars before building settings.
	if d.secretsFile != "" {
		envFromFile, err := parseSecretsFile(d.secretsFile)
		if err != nil {
			return fmt.Errorf("read secrets file: %w", err)
		}
		for k, v := range envFromFile {
			d.flags.env = append(d.flags.env, fmt.Sprintf("%s=%s", k, v))
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
			app := existing
			return d.deployApp(ctx, app, host, name)
		}
	}
	settings.Name = name
	settings.Host = host

	app := docker.NewApplication(ns, settings)

	err = d.deployApp(ctx, app, host, name)
	if d.jsonOutput {
		if err != nil {
			out, _ := json.Marshal(map[string]string{"error": err.Error(), "status": "error"})
			fmt.Println(string(out))
			return err
		}
		out, _ := json.Marshal(map[string]string{"version": imageRef, "status": "ok"})
		fmt.Println(string(out))
		return nil
	}
	return err
}

func (d *deployCommand) deployApp(ctx context.Context, app *docker.Application, host, name string) error {
	return runWithProgress("Deploying "+host, func(progress docker.DeployProgressCallback) error {
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
	})
}

func parseSecretsFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" {
			continue
		}
		out[k] = v
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func applyProxyBind(ns *docker.Namespace, bind string) error {
	if bind == "" {
		return nil
	}
	// Validate and store for proxy.Boot to read via env.
	// Expected format 127.0.0.1:8080 or :8080
	if strings.Contains(bind, ":") {
		parts := strings.Split(bind, ":")
		port := parts[len(parts)-1]
		if port == "" {
			return fmt.Errorf("invalid --proxy-bind %q: missing port", bind)
		}
		os.Setenv("OMAHAB_PROXY_BIND", bind)
		_ = port
	} else {
		os.Setenv("OMAHAB_PROXY_BIND", bind)
	}
	_ = ns
	return nil
}
