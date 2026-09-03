package client

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type webAppDef struct {
	Name      string
	Subdomain string
	Slug      string
	Icon      string
}

var omahabWebApps = []webAppDef{
	{Name: "Omahab Photos", Subdomain: "photos", Slug: "photos", Icon: "photo"},
	{Name: "Omahab Docs", Subdomain: "docs", Slug: "docs", Icon: "document"},
	{Name: "Omahab Save", Subdomain: "save", Slug: "save", Icon: "bookmark"},
	{Name: "Omahab AI", Subdomain: "ai", Slug: "ai", Icon: "ai"},
	{Name: "Omahab Git", Subdomain: "git", Slug: "git", Icon: "git"},
	{Name: "Omahab CI", Subdomain: "ci", Slug: "ci", Icon: "ci"},
	{Name: "Omahab Home", Subdomain: "home", Slug: "home", Icon: "home"},
}

// appURL derives the URL for a given subdomain from the server URL.
// When the server host is an IP or localhost, it falls back to the server URL itself
// (private-only mode would use ports but we keep it simple until B4 lands).
func appURL(serverURL, subdomain string) string {
	if strings.TrimSpace(serverURL) == "" {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(serverURL))
	if err != nil || u.Host == "" {
		return strings.TrimRight(strings.TrimSpace(serverURL), "/")
	}
	host := u.Hostname()
	if host == "" {
		return strings.TrimRight(serverURL, "/")
	}
	if net.ParseIP(host) != nil || host == "localhost" || host == "127.0.0.1" {
		// No domain — fallback to server URL for Home, and same for others (B4 port logic not yet)
		// For Home, just return server URL; for others, return server URL with path hint
		if subdomain == "home" {
			return strings.TrimRight(serverURL, "/")
		}
		// For IP-backed installs, still try to give a distinct URL via subpath? Keep server URL.
		return strings.TrimRight(serverURL, "/")
	}
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return strings.TrimRight(serverURL, "/")
	}
	suffix := strings.Join(parts[1:], ".")
	scheme := u.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return scheme + "://" + subdomain + "." + suffix
}

// SyncWebApps registers web-app entries for each Omahab app.
// It prefers omarchy-webapp-install if present, else falls back to .desktop files.
func SyncWebApps(serverURL string) error {
	if strings.TrimSpace(serverURL) == "" {
		return fmt.Errorf("server_url not configured")
	}
	installer := findOmarchyInstaller()
	if installer != "" {
		var lastErr error
		for _, app := range omahabWebApps {
			u := appURL(serverURL, app.Subdomain)
			if u == "" {
				continue
			}
			// Try installer with name, url, icon
			cmd := exec.Command(installer, app.Name, u, app.Icon)
			// Best-effort; ignore errors but capture last
			if err := cmd.Run(); err != nil {
				// Try alternative signature without icon
				cmd2 := exec.Command(installer, app.Name, u)
				if err2 := cmd2.Run(); err2 != nil {
					lastErr = err
				}
			}
		}
		if lastErr != nil {
			// Fallback to desktop files if installer failed for at least one
			return syncDesktopFiles(serverURL)
		}
		return nil
	}
	return syncDesktopFiles(serverURL)
}

// RemoveWebApps removes previously registered web-app entries.
func RemoveWebApps() error {
	installer := findOmarchyInstaller()
	if installer != "" {
		// Try uninstaller variant if exists
		uninstaller := strings.TrimSuffix(installer, "-install") + "-uninstall"
		if _, err := exec.LookPath(uninstaller); err == nil {
			for _, app := range omahabWebApps {
				_ = exec.Command(uninstaller, app.Name).Run()
			}
		} else {
			// Installer may handle idempotent re-sync; fallback to desktop removal
			for _, app := range omahabWebApps {
				_ = exec.Command(installer, "--remove", app.Name).Run()
			}
		}
	}
	// Always clean desktop files as well
	return removeDesktopFiles()
}

func findOmarchyInstaller() string {
	candidates := []string{"omarchy-webapp-install", "omarchy-webapp-install.sh"}
	for _, name := range candidates {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	// Also check common Omarchy path
	if _, err := os.Stat("/usr/local/bin/omarchy-webapp-install"); err == nil {
		return "/usr/local/bin/omarchy-webapp-install"
	}
	return ""
}

func desktopDir() string {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "applications")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
		if home == "" {
			return ""
		}
	}
	return filepath.Join(home, ".local", "share", "applications")
}

func syncDesktopFiles(serverURL string) error {
	dir := desktopDir()
	if dir == "" {
		return fmt.Errorf("cannot determine desktop dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	browserCmd := detectBrowserCommand()
	for _, app := range omahabWebApps {
		u := appURL(serverURL, app.Subdomain)
		if u == "" {
			continue
		}
		filename := filepath.Join(dir, fmt.Sprintf("omahab-%s.desktop", app.Slug))
		content := fmt.Sprintf(`[Desktop Entry]
Name=%s
Comment=Omahab %s on %s
Exec=%s
Type=Application
Icon=%s
Categories=Network;Office;
StartupWMClass=chromium
Terminal=false
`, app.Name, app.Subdomain, hostSuffix(serverURL), browserExecLine(browserCmd, u), desktopIcon(app.Slug))
		tmp := filename + ".tmp"
		if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, filename); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	return nil
}

func removeDesktopFiles() error {
	dir := desktopDir()
	if dir == "" {
		return nil
	}
	var lastErr error
	for _, app := range omahabWebApps {
		filename := filepath.Join(dir, fmt.Sprintf("omahab-%s.desktop", app.Slug))
		if err := os.Remove(filename); err != nil && !os.IsNotExist(err) {
			lastErr = err
		}
	}
	return lastErr
}

func hostSuffix(serverURL string) string {
	u, err := url.Parse(serverURL)
	if err != nil || u.Host == "" {
		return serverURL
	}
	host := u.Hostname()
	if net.ParseIP(host) != nil {
		return host
	}
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return host
	}
	return strings.Join(parts[1:], ".")
}

func detectBrowserCommand() string {
	candidates := []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "brave", "brave-browser", "firefox"}
	for _, c := range candidates {
		if _, err := exec.LookPath(c); err == nil {
			return c
		}
	}
	return "xdg-open"
}

func browserExecLine(cmd, urlStr string) string {
	switch cmd {
	case "xdg-open":
		return fmt.Sprintf("xdg-open %s", shellEscape(urlStr))
	default:
		// chromium --app
		return fmt.Sprintf("%s --app=%s", cmd, shellEscape(urlStr))
	}
}

func desktopIcon(slug string) string {
	// Use generic icons that exist on most desktops; Omarchy may have custom icons but fallback is fine
	switch slug {
	case "photos":
		return "image-x-generic"
	case "docs":
		return "x-office-document"
	case "save":
		return "bookmark-new"
	case "ai":
		return "applications-science"
	case "git":
		return "vcs-normal"
	case "ci":
		return "applications-engineering"
	case "home":
		return "go-home"
	default:
		return "web-browser"
	}
}

func shellEscape(s string) string {
	// Simple quoting for URLs without spaces (always safe)
	if strings.ContainsAny(s, " \t\n\"'\\") {
		return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
	}
	return s
}
