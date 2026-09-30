// Package drops routes files from the drops inbox to application
// consume/staging directories by extension. It is deliberately dumb:
// no AI, no content sniffing, no recursion. Unknown files stay put.
package drops

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Target is where a file belongs.
type Target string

const (
	// TargetKeep leaves the file in the inbox (unknown type, dotfile, dir).
	TargetKeep Target = "keep"
	// TargetPaperless is the Paperless-ngx consume directory.
	TargetPaperless Target = "paperless"
	// TargetPhotos stages camera photos/video for Immich import.
	TargetPhotos Target = "photos"
	// TargetMedia stages library audio/video for Jellyfin.
	TargetMedia Target = "media"
)

var (
	paperlessExts = map[string]bool{
		".pdf": true, ".tif": true, ".tiff": true, ".txt": true, ".csv": true,
		".md": true, ".docx": true, ".doc": true, ".xlsx": true, ".xls": true,
		".pptx": true, ".odt": true, ".ods": true, ".eml": true,
	}
	photosExts = map[string]bool{
		".jpg": true, ".jpeg": true, ".png": true, ".heic": true, ".heif": true,
		".webp": true, ".gif": true, ".bmp": true, ".dng": true, ".raw": true,
		".arw": true, ".cr2": true, ".nef": true, ".rw2": true,
		".mp4": true, ".mov": true, ".3gp": true,
	}
	mediaExts = map[string]bool{
		".mkv": true, ".avi": true, ".webm": true, ".m2ts": true, ".ts": true,
		".m4v": true, ".mp3": true, ".flac": true, ".ogg": true, ".opus": true,
		".wav": true, ".aac": true, ".m4a": true, ".m4b": true,
	}
)

// Classify returns the routing target for a file name.
func Classify(name string) Target {
	base := strings.TrimSpace(filepath.Base(name))
	if base == "" || strings.HasPrefix(base, ".") {
		return TargetKeep
	}
	ext := strings.ToLower(filepath.Ext(base))
	if ext == "" {
		return TargetKeep
	}
	switch {
	case paperlessExts[ext]:
		return TargetPaperless
	case photosExts[ext]:
		return TargetPhotos
	case mediaExts[ext]:
		return TargetMedia
	default:
		return TargetKeep
	}
}

// Destinations maps non-keep targets to directories.
type Destinations struct {
	Paperless string
	Photos    string
	Media     string
}

// Result describes one routed file.
type Result struct {
	File   string `json:"file"`
	Target Target `json:"target"`
	To     string `json:"to"`
}

// RouteInbox moves top-level regular files out of the inbox to their
// destination directories. It skips subdirectories, symlinks, dotfiles,
// and unknown types. Collisions get a -N suffix. Moved files are chmodded
// 0644 so service users (paperless, syncthing) can read them regardless
// of which user runs the router.
func RouteInbox(inbox string, dests Destinations) ([]Result, error) {
	for _, dir := range []string{inbox, dests.Paperless, dests.Photos, dests.Media} {
		if strings.TrimSpace(dir) == "" {
			return nil, fmt.Errorf("destination directory is required")
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	entries, err := os.ReadDir(inbox)
	if err != nil {
		return nil, fmt.Errorf("read inbox: %w", err)
	}
	var out []Result
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if !e.Type().IsRegular() {
			continue
		}
		var targetDir string
		switch Classify(name) {
		case TargetPaperless:
			targetDir = dests.Paperless
		case TargetPhotos:
			targetDir = dests.Photos
		case TargetMedia:
			targetDir = dests.Media
		default:
			continue
		}
		to := filepath.Join(targetDir, name)
		if _, err := os.Stat(to); err == nil {
			to = uniqueName(targetDir, name)
		}
		from := filepath.Join(inbox, name)
		if err := os.Rename(from, to); err != nil {
			continue
		}
		_ = os.Chmod(to, 0o644)
		out = append(out, Result{File: name, Target: Classify(name), To: to})
	}
	return out, nil
}

func uniqueName(dir, name string) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, i, ext))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}
