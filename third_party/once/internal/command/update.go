package command

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/basecamp/once/internal/docker"
)

type updateCommand struct {
	cmd         *cobra.Command
	flags       settingsFlags
	image       string
	jsonOutput  bool
	secretsFile string
	proxyBind   string
	tlsMode     string
}

func newUpdateCommand() *updateCommand {
	u := &updateCommand{}
	u.cmd = &cobra.Command{
		Use:   "update <host>",
		Short: "Update settings for a deployed application",
		Args:  cobra.ExactArgs(1),
		RunE:  WithNamespace(u.run),
	}

	u.flags.register(u.cmd)
	u.cmd.Flags().StringVar(&u.image, "image", "", "new image for the application")
	u.cmd.Flags().BoolVar(&u.jsonOutput, "json", false, "output JSON")
	u.cmd.Flags().StringVar(&u.secretsFile, "secrets-file", "", "path to KEY=VAL secrets file (merged into env)")
	u.cmd.Flags().StringVar(&u.proxyBind, "proxy-bind", "", "loopback bind for kamal-proxy (e.g. 127.0.0.1:8080)")
	u.cmd.Flags().StringVar(&u.tlsMode, "tls", "", "TLS mode: external (Caddy owns TLS) or internal")

	return u
}

// Private

func (u *updateCommand) run(ctx context.Context, ns *docker.Namespace, cmd *cobra.Command, args []string) error {
	currentHost := args[0]

	app, err := findApplication(ns, currentHost)
	if err != nil {
		return err
	}

	image := app.Settings.Image
	if cmd.Flags().Changed("image") {
		image = u.image
	}

	// Mirror deploy: proxy-bind configuration applies before ns.Setup, and
	// secrets-file/TLS settings apply even without explicit --env/--disable-tls.
	if err := applyProxyBind(u.proxyBind); err != nil {
		return err
	}
	secretsMerged := false
	if u.secretsFile != "" {
		if err := mergeSecretsFile(&u.flags, u.secretsFile); err != nil {
			return err
		}
		secretsMerged = true
	}
	tlsExternal := u.tlsMode == "external"
	if tlsExternal {
		u.flags.disableTLS = true
	}

	if err := ns.Setup(ctx); err != nil {
		return fmt.Errorf("%w: %w", docker.ErrSetupFailed, err)
	}

	settings, err := u.flags.applyChanges(cmd, app.Settings, image, secretsMerged, tlsExternal)
	if err != nil {
		return err
	}

	if settings.Host != app.Settings.Host {
		if ns.HostInUseByAnother(settings.Host, app.Settings.Name) {
			if u.jsonOutput {
				if encErr := emitJSON(cmd, deployResult{Status: "error", Error: docker.ErrHostnameInUse.Error()}); encErr != nil {
					return encErr
				}
			}
			return docker.ErrHostnameInUse
		}
	}

	oldSettings := app.Settings
	app.Settings = settings

	deploy := func(progress docker.DeployProgressCallback) error {
		if err := app.Deploy(ctx, progress); err != nil {
			app.Settings = oldSettings
			return fmt.Errorf("%w: %w", docker.ErrDeployFailed, err)
		}
		return nil
	}
	if u.jsonOutput {
		// No interactive progress wrapper: one JSON record on stdout.
		err = deploy(func(docker.DeployProgress) {})
	} else {
		err = runWithProgress("Updating "+currentHost, deploy)
	}
	if u.jsonOutput {
		out := deployResult{Status: "ok", Version: settings.Image}
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
