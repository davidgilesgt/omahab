package installer

import (
	"bytes"
	"context"
	"fmt"
	"strings"
)

const (
	tailscaleKeyringPath  = "/usr/share/keyrings/tailscale-archive-keyring.gpg"
	cloudflareKeyringPath = "/usr/share/keyrings/cloudflare-main.gpg"
	tailscaleSourcePath   = "/etc/apt/sources.list.d/omahab-tailscale.list"
	cloudflareSourcePath  = "/etc/apt/sources.list.d/omahab-cloudflared.list"
	autoUpgradesPath      = "/etc/apt/apt.conf.d/20auto-upgrades"

	cloudflareKeyringURL = "https://pkg.cloudflare.com/cloudflare-main.gpg"
	autoUpgradesContent  = "APT::Periodic::Update-Package-Lists \"1\";\nAPT::Periodic::Unattended-Upgrade \"1\";\n"
)

// PackagesForOS returns the apt package list for the given OS id.
// It includes both distro and vendor packages (tailscale, cloudflared) that
// are installed via the omahab-managed vendor repos. The caller is
// responsible for ensuring the vendor repos are configured before install.
func PackagesForOS(osID string) ([]string, error) {
	switch strings.ToLower(strings.TrimSpace(osID)) {
	case "debian":
		return []string{
			"ca-certificates",
			"docker.io",
			"docker-compose",
			"nftables",
			"unattended-upgrades",
			"tailscale",
			"cloudflared",
		}, nil
	case "ubuntu":
		return []string{
			"ca-certificates",
			"docker.io",
			"docker-compose-v2",
			"nftables",
			"unattended-upgrades",
			"tailscale",
			"cloudflared",
		}, nil
	default:
		return nil, fmt.Errorf("unsupported OS %q: need debian or ubuntu", osID)
	}
}

func (s *Service) runPackagesStep(ctx context.Context, opts InstallOptions) RunResult {
	emit := func(line string) {
		if opts.Emit != nil {
			opts.Emit(StepLog{Step: StepPackages, Line: line})
		}
	}
	failed := func(err error) RunResult {
		return RunResult{Step: StepPackages, Status: JournalFailed, Error: err.Error()}
	}

	// 1. Resolve OS via probes.OSRelease (fail if unknown os/codename).
	if s.probes.OSRelease == nil {
		return failed(fmt.Errorf("os release probe not configured"))
	}
	info, err := s.probes.OSRelease()
	if err != nil {
		return failed(fmt.Errorf("resolve OS: %w", err))
	}
	osID := strings.ToLower(strings.TrimSpace(info.ID))
	codename := strings.ToLower(strings.TrimSpace(info.Codename))

	switch osID {
	case "debian":
		if codename != "trixie" {
			return failed(fmt.Errorf("unsupported debian codename %q: need trixie", info.Codename))
		}
	case "ubuntu":
		if codename != "resolute" {
			return failed(fmt.Errorf("unsupported ubuntu codename %q: need resolute", info.Codename))
		}
	default:
		return failed(fmt.Errorf("unsupported OS %q: need debian trixie or ubuntu resolute", info.ID))
	}
	emit(fmt.Sprintf("resolved OS %s %s", osID, codename))

	tailscaleKeyURL := fmt.Sprintf("https://pkgs.tailscale.com/stable/%s/%s.noarmor.gpg", osID, codename)
	tailscaleSourceLine := fmt.Sprintf("deb [signed-by=/usr/share/keyrings/tailscale-archive-keyring.gpg] https://pkgs.tailscale.com/stable/%s %s main", osID, codename)
	cloudflareSourceLine := "deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main"

	// 2. Download keyrings — skip when file already exists.
	if s.probes.FileExists == nil {
		return failed(fmt.Errorf("file exists probe not configured"))
	}
	if s.probes.DownloadFile == nil {
		return failed(fmt.Errorf("download file probe not configured"))
	}
	if !s.probes.FileExists(tailscaleKeyringPath) {
		emit("downloading tailscale keyring")
		if err := s.probes.DownloadFile(ctx, tailscaleKeyURL, tailscaleKeyringPath); err != nil {
			return failed(fmt.Errorf("download tailscale keyring: %w", err))
		}
		emit("downloaded tailscale keyring")
	} else {
		emit("tailscale keyring already present")
	}
	if !s.probes.FileExists(cloudflareKeyringPath) {
		emit("downloading cloudflare keyring")
		if err := s.probes.DownloadFile(ctx, cloudflareKeyringURL, cloudflareKeyringPath); err != nil {
			return failed(fmt.Errorf("download cloudflare keyring: %w", err))
		}
		emit("downloaded cloudflare keyring")
	} else {
		emit("cloudflare keyring already present")
	}

	// 3. Write apt source files — skip when existing content is byte-identical.
	if s.probes.ReadFile == nil {
		return failed(fmt.Errorf("read file probe not configured"))
	}
	if s.probes.WriteFile == nil {
		return failed(fmt.Errorf("write file probe not configured"))
	}

	writeSource := func(path, line string) error {
		desired := line + "\n"
		existing, err := s.probes.ReadFile(path)
		if err == nil && bytes.Equal(existing, []byte(desired)) {
			return nil
		}
		if err := s.probes.WriteFile(path, []byte(desired), 0o644); err != nil {
			return err
		}
		return nil
	}

	emit("writing apt source for tailscale")
	if err := writeSource(tailscaleSourcePath, tailscaleSourceLine); err != nil {
		return failed(fmt.Errorf("write tailscale source: %w", err))
	}
	emit("writing apt source for cloudflared")
	if err := writeSource(cloudflareSourcePath, cloudflareSourceLine); err != nil {
		return failed(fmt.Errorf("write cloudflared source: %w", err))
	}

	// 4. APT refresh.
	if s.probes.APTRefresh == nil {
		return failed(fmt.Errorf("apt refresh probe not configured"))
	}
	emit("running apt update")
	if err := s.probes.APTRefresh(ctx); err != nil {
		return failed(fmt.Errorf("apt refresh: %w", err))
	}
	emit("apt update completed")

	// 5. APT install.
	if s.probes.APTInstall == nil {
		return failed(fmt.Errorf("apt install probe not configured"))
	}
	pkgs, err := PackagesForOS(osID)
	if err != nil {
		return failed(err)
	}
	emit(fmt.Sprintf("installing packages: %s", strings.Join(pkgs, ", ")))
	if err := s.probes.APTInstall(ctx, pkgs...); err != nil {
		return failed(fmt.Errorf("apt install: %w", err))
	}
	emit("package installation completed")

	// 6. Write 20auto-upgrades — skip when identical.
	{
		desired := autoUpgradesContent
		existing, err := s.probes.ReadFile(autoUpgradesPath)
		if err != nil || !bytes.Equal(existing, []byte(desired)) {
			if s.probes.WriteFile == nil {
				return failed(fmt.Errorf("write file probe not configured"))
			}
			emit("writing unattended-upgrades config")
			if err := s.probes.WriteFile(autoUpgradesPath, []byte(desired), 0o644); err != nil {
				return failed(fmt.Errorf("write auto-upgrades config: %w", err))
			}
		} else {
			emit("unattended-upgrades config already present")
		}
	}

	// 7. Ensure cloudflared system user and data dir (for User=cloudflared unit).
	{
		// Check if user exists via LookupUser or getent.
		userExists := false
		if s.probes.LookupUser != nil {
			if _, _, _, err := s.probes.LookupUser("cloudflared"); err == nil {
				userExists = true
			}
		} else if s.probes.CommandOutput != nil {
			if _, err := s.probes.CommandOutput(ctx, "getent", "passwd", "cloudflared"); err == nil {
				userExists = true
			}
		}
		if !userExists {
			emit("creating cloudflared system user")
			if s.probes.CommandOutput != nil {
				if _, err := s.probes.CommandOutput(ctx, "useradd", "--system", "--home-dir", "/var/lib/cloudflared", "--create-home", "--shell", "/usr/sbin/nologin", "cloudflared"); err != nil {
					emit(fmt.Sprintf("useradd cloudflared failed (best-effort): %v", err))
				} else {
					emit("created cloudflared user")
				}
			}
		} else {
			emit("cloudflared user already present")
		}
		// Ensure /var/lib/cloudflared exists with correct perms.
		if s.probes.CommandOutput != nil {
			if _, err := s.probes.CommandOutput(ctx, "install", "-d", "-m", "0700", "-o", "cloudflared", "-g", "cloudflared", "/var/lib/cloudflared"); err != nil {
				// Fallback via MkdirAll + Chown if install fails or user not yet resolvable.
				if s.probes.MkdirAll != nil {
					_ = s.probes.MkdirAll("/var/lib/cloudflared", 0o700)
				}
				if s.probes.LookupUser != nil {
					if uid, gid, _, err2 := s.probes.LookupUser("cloudflared"); err2 == nil {
						if s.probes.Chown != nil {
							_ = s.probes.Chown("/var/lib/cloudflared", uid, gid)
						}
					}
				}
			}
		} else if s.probes.MkdirAll != nil {
			_ = s.probes.MkdirAll("/var/lib/cloudflared", 0o700)
			if s.probes.LookupUser != nil && s.probes.Chown != nil {
				if uid, gid, _, err := s.probes.LookupUser("cloudflared"); err == nil {
					_ = s.probes.Chown("/var/lib/cloudflared", uid, gid)
				}
			}
		}
	}

	return RunResult{Step: StepPackages, Status: JournalCompleted}
}

// RollbackPackages leaves packages installed; removing Docker or vendor sources
// on rollback would destroy state and break recovery. It is a documented no-op
// returning nil.
func RollbackPackages(ctx context.Context, p Probes) error {
	_ = ctx
	_ = p
	return nil
}
