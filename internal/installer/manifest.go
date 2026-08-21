package installer

import (
	"encoding/json"
	"fmt"
	"time"
)

// Manifest is written to /var/lib/omahab/install-manifest.json on success.
type Manifest struct {
	Version     string         `json:"version"`
	InstalledAt time.Time      `json:"installed_at"`
	OS          OSInfo         `json:"os"`
	Arch        string         `json:"arch"`
	Steps       []JournalEntry `json:"steps"`
	SSHKeys     []SSHKey       `json:"ssh_keys,omitempty"`
	Preflight   []CheckResult  `json:"preflight"`
}

// WriteManifest renders the manifest as JSON and writes it via probes.
func WriteManifest(probes Probes, m Manifest) error {
	if probes.WriteFile == nil {
		return fmt.Errorf("write file probe not configured")
	}
	// Ensure UTC
	m.InstalledAt = m.InstalledAt.UTC()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := "/var/lib/omahab/install-manifest.json"
	if err := probes.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

// ReadManifest reads the manifest via probes.
func ReadManifest(probes Probes) (*Manifest, error) {
	if probes.ReadFile == nil {
		return nil, fmt.Errorf("read file probe not configured")
	}
	data, err := probes.ReadFile("/var/lib/omahab/install-manifest.json")
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
