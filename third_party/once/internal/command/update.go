package command

import (
	"context"
	"encoding/json"
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

	if err := ns.Setup(ctx); err != nil {
		return fmt.Errorf("%w: %w", docker.ErrSetupFailed, err)
	}

	// Mirror deploy: loopback proxy bind, secrets file merged into env, external TLS.
	if u.proxyBind != "" {
		if err := applyProxyBind(ns, u.proxyBind); err != nil {
			return err
		}
	}
	if u.secretsFile != "" {
		envFromFile, err := parseSecretsFile(u.secretsFile)
		if err != nil {
			return fmt.Errorf("read secrets file: %w", err)
		}
		for k, v := range envFromFile {
			u.flags.env = append(u.flags.env, fmt.Sprintf("%s=%s", k, v))
		}
	}
	if u.tlsMode == "external" {
		u.flags.disableTLS = true
	}

	settings, err := u.flags.applyChanges(cmd, app.Settings, image)
	if err != nil {
		return err
	}

	if settings.Host != app.Settings.Host {
		if ns.HostInUseByAnother(settings.Host, app.Settings.Name) {
			if u.jsonOutput {
				out, _ := json.Marshal(map[string]string{"error": docker.ErrHostnameInUse.Error(), "status": "error"})
				fmt.Println(string(out))
			}
			return docker.ErrHostnameInUse
		}
	}

	oldSettings := app.Settings
	app.Settings = settings

	err = runWithProgress("Updating "+currentHost, func(progress docker.DeployProgressCallback) error {
		if err := app.Deploy(ctx, progress); err != nil {
			app.Settings = oldSettings
			return fmt.Errorf("%w: %w", docker.ErrDeployFailed, err)
		}
		return nil
	})
	if u.jsonOutput {
		if err != nil {
			out, _ := json.Marshal(map[string]string{"error": err.Error(), "status": "error"})
			fmt.Println(string(out))
			return err
		}
		out, _ := json.Marshal(map[string]string{"version": settings.Image, "status": "ok"})
		fmt.Println(string(out))
		return nil
	}
	return err
}
