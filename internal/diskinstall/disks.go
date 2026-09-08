package diskinstall

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Disk describes a block device from backend --list-disks.
type Disk struct {
	Path           string `json:"path"`
	Identity       string `json:"identity"`
	SizeBytes      int64  `json:"size_bytes"`
	Model          string `json:"model"`
	Serial         string `json:"serial"`
	Transport      string `json:"transport"`
	External       bool   `json:"external"`
	SystemEligible bool   `json:"system_eligible"`
	DataEligible   bool   `json:"data_eligible"`
	SystemReason   string `json:"system_reason"`
	DataReason     string `json:"data_reason"`
}

// ListDisksResponse is the JSON output of `omahab-install-disk --list-disks`.
type ListDisksResponse struct {
	Disks []Disk `json:"disks"`
}

// ParseListDisks parses JSON output from --list-disks and sorts by identity then path.
func ParseListDisks(data []byte) (*ListDisksResponse, error) {
	var resp ListDisksResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse list-disks: %w", err)
	}
	if resp.Disks == nil {
		resp.Disks = []Disk{}
	}
	sort.Slice(resp.Disks, func(i, j int) bool {
		if resp.Disks[i].Identity != resp.Disks[j].Identity {
			return resp.Disks[i].Identity < resp.Disks[j].Identity
		}
		return resp.Disks[i].Path < resp.Disks[j].Path
	})
	return &resp, nil
}

// FormatSize returns human readable size.
func FormatSize(bytes int64) string {
	if bytes >= 1024*1024*1024 {
		gb := float64(bytes) / (1024 * 1024 * 1024)
		if gb == float64(int64(gb)) {
			return fmt.Sprintf("%d GiB", int64(gb))
		}
		return fmt.Sprintf("%.1f GiB", gb)
	}
	if bytes >= 1024*1024 {
		mb := float64(bytes) / (1024 * 1024)
		return fmt.Sprintf("%.0f MiB", mb)
	}
	return fmt.Sprintf("%d B", bytes)
}

// DiskDisplay returns a short display line for a disk.
func DiskDisplay(d Disk) string {
	serial := d.Serial
	if serial == "" {
		serial = "serial unavailable"
	}
	model := strings.TrimSpace(d.Model)
	if model == "" {
		model = "unknown model"
	}
	external := ""
	if d.External {
		external = " (external)"
	}
	return fmt.Sprintf("%s  %s  %s  %s%s", d.Path, model, serial, FormatSize(d.SizeBytes), external)
}

// RecommendSystemDisk picks the smallest system-eligible internal disk, or if none,
// smallest opted-in external system-eligible disk if allowExternal. Returns nil if none candidate.
// Caller passes all disks and externalOptIn map (path -> true if user opted in).
func RecommendSystemDisk(disks []Disk, externalOptIn map[string]bool) *Disk {
	var candidates []Disk
	for _, d := range disks {
		if !d.SystemEligible {
			continue
		}
		if d.External && !externalOptIn[d.Path] {
			continue
		}
		candidates = append(candidates, d)
	}
	if len(candidates) == 0 {
		return nil
	}
	// Already sorted by identity then path, but we need smallest size.
	// Prefer smallest system-eligible; if tie, identity/path order.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].SizeBytes != candidates[j].SizeBytes {
			return candidates[i].SizeBytes < candidates[j].SizeBytes
		}
		if candidates[i].Identity != candidates[j].Identity {
			return candidates[i].Identity < candidates[j].Identity
		}
		return candidates[i].Path < candidates[j].Path
	})
	return &candidates[0]
}

// SelectedDisks returns disks selected for installation: system disk + all data-eligible internal
// plus opted-in external data-eligible. Caller must have chosen system disk.
func SelectedDisks(disks []Disk, systemPath string, externalOptIn map[string]bool) []Disk {
	var selected []Disk
	var sys *Disk
	for i := range disks {
		if disks[i].Path == systemPath {
			sys = &disks[i]
			break
		}
	}
	if sys != nil {
		selected = append(selected, *sys)
	}
	for _, d := range disks {
		if d.Path == systemPath {
			continue
		}
		if !d.DataEligible {
			continue
		}
		if d.External && !externalOptIn[d.Path] {
			continue
		}
		selected = append(selected, d)
	}
	// Sort selected by system first then stable order identity/path for data disks
	if len(selected) > 1 {
		sysDisk := selected[0]
		data := selected[1:]
		sort.Slice(data, func(i, j int) bool {
			if data[i].Identity != data[j].Identity {
				return data[i].Identity < data[j].Identity
			}
			return data[i].Path < data[j].Path
		})
		selected = append([]Disk{sysDisk}, data...)
	}
	return selected
}

// DataDisks returns data-eligible disks excluding the system disk.
func DataDisks(disks []Disk, systemPath string, externalOptIn map[string]bool) []Disk {
	var out []Disk
	for _, d := range disks {
		if d.Path == systemPath {
			continue
		}
		if !d.DataEligible {
			continue
		}
		if d.External && !externalOptIn[d.Path] {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Identity != out[j].Identity {
			return out[i].Identity < out[j].Identity
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// ErasePhrase returns the required confirmation literal for N disks.
func ErasePhrase(n int) string {
	return fmt.Sprintf("ERASE %d DISKS", n)
}

// SelectionFileData is written to --selection-file (root-owned 0600).
// Contains ordered chosen disks exactly as from --list-disks (same records).
type SelectionFileData struct {
	Disks []Disk `json:"disks"`
}

// NetworkFile is the JSON passed via --network-file.
type NetworkFile struct {
	Mode           string `json:"mode"` // "profile" or "wired-dhcp"
	ConnectionUUID string `json:"connection_uuid"`
	Interface      string `json:"interface"`
	Keyfile        string `json:"keyfile"` // staged path for profile, empty for wired-dhcp
}

// ProgressEvent is the JSON line emitted to --progress-fd.
type ProgressEvent struct {
	Stage   string `json:"stage"`  // preflight|partition|format|mount|configure|install|account|unmount|done
	Status  string `json:"status"` // running|complete|failed
	Message string `json:"message"`
}
