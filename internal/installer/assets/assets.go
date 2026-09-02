package assets

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

//go:embed all:root
var embedded embed.FS

// requiredFiles lists the mandatory asset paths relative to the asset root.
// Catalog is handled separately (requires at least one file under catalog/).
// Web is optional and not listed here.
var requiredFiles = []string{
	"bin/omahab",
	"bin/omahabd",
	"systemd/omahabd.service",
	"systemd/omahab-builder.socket",
	"systemd/omahab-builder.service",
	"systemd/omahab-builder-prune.service",
	"systemd/omahab-builder-prune.timer",
	"systemd/omahab-backup.service",
	"systemd/omahab-backup.timer",
	"systemd/omahab-verify.service",
	"systemd/omahab-verify.timer",
	"systemd/cloudflared.service",
	"tmpfiles.d/omahab.conf",
}

// Load returns the embedded asset filesystem.
//
// The assets are staged by scripts/build.sh into internal/installer/assets/root/
// and embedded via //go:embed all:root. A fresh checkout contains only
// root/.gitkeep, which is detected as a stripped build and returns a
// descriptive error telling the developer to run scripts/build.sh or use
// --asset-dir.
//
// The returned FS is rooted at the asset root so callers see bin/omahab etc
// at the top level (identical layout to LoadDir).
func Load() (fs.FS, error) {
	sub, err := fs.Sub(embedded, "root")
	if err != nil {
		return nil, fmt.Errorf("embedded assets missing root: %w", err)
	}

	// Detect stripped build: only .gitkeep present.
	// This is the state of a fresh checkout before scripts/build.sh has staged assets.
	if _, err := fs.Stat(sub, "bin/omahab"); err != nil && errors.Is(err, fs.ErrNotExist) {
		if _, err2 := fs.Stat(sub, ".gitkeep"); err2 == nil {
			entries, readErr := fs.ReadDir(sub, ".")
			if readErr == nil {
				// If the only entry is .gitkeep (or the directory is effectively empty),
				// report the stripped-build hint.
				onlyGitkeep := false
				if len(entries) == 1 && entries[0].Name() == ".gitkeep" {
					onlyGitkeep = true
				} else if len(entries) == 0 {
					onlyGitkeep = true
				} else {
					// Fallback: if .gitkeep exists but none of the required files do,
					// treat it as stripped for a clear developer message.
					hasRequired := false
					for _, p := range requiredFiles {
						if _, e := fs.Stat(sub, p); e == nil {
							hasRequired = true
							break
						}
					}
					if !hasRequired {
						onlyGitkeep = true
					}
				}
				if onlyGitkeep {
					return nil, fmt.Errorf("installer assets not staged: embedded filesystem contains only .gitkeep; run scripts/build.sh to stage assets or use --asset-dir to load from a directory")
				}
			}
		}
	}

	if err := Validate(sub); err != nil {
		return nil, fmt.Errorf("installer assets incomplete: %w; run scripts/build.sh to stage assets or use --asset-dir", err)
	}
	return sub, nil
}

// LoadDir returns an fs.FS rooted at dir after verifying the asset layout.
//
// It uses os.DirFS and then validates the layout with Validate so callers
// get identical guarantees for embedded and directory-loaded assets.
func LoadDir(dir string) (fs.FS, error) {
	if dir == "" {
		return nil, fmt.Errorf("asset directory not specified: use --asset-dir or run scripts/build.sh")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("asset directory %q not accessible: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("asset directory %q is not a directory", dir)
	}
	fsys := os.DirFS(dir)
	if err := Validate(fsys); err != nil {
		return nil, err
	}
	return fsys, nil
}

// Validate checks that all required asset entries are present, readable, and
// (for bin entries) non-empty, and that catalog contains at least one file.
// Web is optional. It returns a single error listing every missing/problematic
// entry at once for actionable diagnostics.
func Validate(fsys fs.FS) error {
	if fsys == nil {
		return fmt.Errorf("nil filesystem")
	}
	var issues []string
	for _, p := range requiredFiles {
		info, err := fs.Stat(fsys, p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				issues = append(issues, fmt.Sprintf("%s (missing)", p))
			} else {
				issues = append(issues, fmt.Sprintf("%s (unreadable: %v)", p, err))
			}
			continue
		}
		if info.IsDir() {
			issues = append(issues, fmt.Sprintf("%s (is directory, expected file)", p))
			continue
		}
		// Verify readable by opening.
		f, err := fsys.Open(p)
		if err != nil {
			issues = append(issues, fmt.Sprintf("%s (unreadable: %v)", p, err))
			continue
		}
		_ = f.Close()
		if (p == "bin/omahab" || p == "bin/omahabd") && info.Size() == 0 {
			issues = append(issues, fmt.Sprintf("%s (empty file)", p))
		}
	}

	// Catalog must exist and contain at least one file (recursively).
	found := false
	walkErr := fs.WalkDir(fsys, "catalog", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			found = true
		}
		return nil
	})
	if walkErr != nil {
		if errors.Is(walkErr, fs.ErrNotExist) {
			issues = append(issues, "catalog (missing directory)")
		} else {
			issues = append(issues, fmt.Sprintf("catalog (unreadable: %v)", walkErr))
		}
	} else if !found {
		issues = append(issues, "catalog (empty: no files under catalog/)")
	}

	if len(issues) > 0 {
		return fmt.Errorf("missing required asset(s): %s", strings.Join(issues, ", "))
	}
	return nil
}
