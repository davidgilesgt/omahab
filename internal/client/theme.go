package client

import (
	"os"
	"path/filepath"
	"strings"
)

// OmarchyTheme reads the active Omarchy theme name.
// Path is ~/.config/omarchy/current/theme symlink — best-effort.
// Returns empty string when not on Omarchy or theme not set.
func OmarchyTheme() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
		if home == "" {
			return ""
		}
	}
	candidates := []string{
		filepath.Join(home, ".config", "omarchy", "current", "theme"),
		filepath.Join(home, ".config", "omarchy", "current", "theme.conf"),
		filepath.Join(home, ".config", "omarchy", "theme"),
	}
	for _, p := range candidates {
		// Try symlink target basename
		if target, err := os.Readlink(p); err == nil {
			name := filepath.Base(strings.TrimSpace(target))
			// Target may be a directory like themes/tokyo-night or file name
			// Remove extension if present
			name = strings.TrimSuffix(name, filepath.Ext(name))
			name = strings.TrimSpace(name)
			if name != "" && name != "current" {
				return name
			}
			// Also try parent dir name for path like .../themes/tokyo-night/theme.toml
			if dir := filepath.Base(filepath.Dir(target)); dir != "" && dir != "themes" && dir != "current" && dir != "." {
				// Heuristic: if basename was generic, use dir as theme
				if name == "theme" || name == "config" || name == "settings" {
					return strings.TrimSpace(dir)
				}
			}
		}
		// Try reading file content
		if data, err := os.ReadFile(p); err == nil {
			s := strings.TrimSpace(string(data))
			// Content may be theme name or path
			if s != "" {
				// If content contains path, take basename
				if strings.Contains(s, "/") {
					s = filepath.Base(strings.TrimSpace(s))
					s = strings.TrimSuffix(s, filepath.Ext(s))
				}
				// Allow only known theme names plus generic names
				if s != "" {
					return s
				}
			}
		}
	}
	// Fallback: check parent dir listing for current symlink?
	return ""
}

// DashboardURLWithTheme appends #theme=<name> when Omarchy theme is present.
// It preserves existing hash/query fragments.
func DashboardURLWithTheme(base string) string {
	theme := OmarchyTheme()
	if theme == "" {
		return base
	}
	// Validate theme is one of known palettes or reasonable identifier
	// Allow any non-empty without spaces or control chars
	theme = strings.TrimSpace(theme)
	if theme == "" || strings.ContainsAny(theme, " \t\n\r#?&=;") {
		return base
	}
	// Already has theme fragment? Don't duplicate.
	if strings.Contains(base, "theme=") {
		return base
	}
	// Append appropriately: if base already has #, append &, else #
	if strings.Contains(base, "#") {
		// If base ends with #, just append theme
		if strings.HasSuffix(base, "#") {
			return base + "theme=" + theme
		}
		return base + "&theme=" + theme
	}
	return base + "#theme=" + theme
}
