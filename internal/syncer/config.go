package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Syncthing configuration reconciliation (folders + devices).
//
// Reads use per-resource endpoints; writes POST one object to
// /rest/config/folders or /rest/config/devices after merging only managed
// fields over the existing object (unknown fields preserved). Collection PUT
// is never used: upstream removes entries absent from that array.

// FolderSpec is the managed subset of a Syncthing folder object.
type FolderSpec struct {
	ID             string
	Label          string
	Path           string
	Type           string
	DeviceIDs      []string
	IgnorePatterns []string
}

// DeviceSpec is the managed subset of a Syncthing device object.
type DeviceSpec struct {
	ID   string
	Name string
}

// FolderInfo is one folder object read back from Syncthing.
type FolderInfo struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Path    string   `json:"path"`
	Type    string   `json:"type"`
	Devices []string `json:"-"`
	Raw     map[string]any
}

// DeviceInfo is one device object read back from Syncthing.
type DeviceInfo struct {
	ID   string `json:"deviceID"`
	Name string `json:"name"`
	Raw  map[string]any
}

func (c *HTTPClient) getJSON(ctx context.Context, path string, out any) (int, error) {
	req, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, fmt.Errorf("syncthing %s %s: %d: %s", "GET", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode syncthing %s: %w", path, err)
		}
	}
	return resp.StatusCode, nil
}

func (c *HTTPClient) postJSON(ctx context.Context, path string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := c.request(ctx, http.MethodPost, path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(raw))
	req.Body = io.NopCloser(bytes.NewReader(raw))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("syncthing %s %s: %d: %s", "POST", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (c *HTTPClient) deleteConfig(ctx context.Context, path string) error {
	req, err := c.request(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("syncthing %s %s: %d", "DELETE", path, resp.StatusCode)
	}
	return nil
}

func deviceIDsOf(raw map[string]any) []string {
	var out []string
	devs, _ := raw["devices"].([]any)
	for _, d := range devs {
		if m, ok := d.(map[string]any); ok {
			if id, ok := m["deviceID"].(string); ok && strings.TrimSpace(id) != "" {
				out = append(out, strings.TrimSpace(id))
			}
		}
	}
	return out
}

func parseFolder(raw map[string]any) FolderInfo {
	fi := FolderInfo{Raw: raw}
	if id, ok := raw["id"].(string); ok {
		fi.ID = strings.TrimSpace(id)
	}
	if label, ok := raw["label"].(string); ok {
		fi.Label = label
	}
	if path, ok := raw["path"].(string); ok {
		fi.Path = path
	}
	if typ, ok := raw["type"].(string); ok {
		fi.Type = typ
	}
	fi.Devices = deviceIDsOf(raw)
	return fi
}

// GetFolder reads one folder object by wire ID.
func (c *HTTPClient) GetFolder(ctx context.Context, id string) (FolderInfo, error) {
	var raw map[string]any
	if _, err := c.getJSON(ctx, "/rest/config/folders/"+id, &raw); err != nil {
		return FolderInfo{}, err
	}
	return parseFolder(raw), nil
}

// ListFolders reads every folder object.
func (c *HTTPClient) ListFolders(ctx context.Context) ([]FolderInfo, error) {
	var raws []map[string]any
	if _, err := c.getJSON(ctx, "/rest/config/folders", &raws); err != nil {
		return nil, err
	}
	out := make([]FolderInfo, 0, len(raws))
	for _, raw := range raws {
		out = append(out, parseFolder(raw))
	}
	return out, nil
}

// EnsureFolder merges the managed fields over the existing object (when any)
// and POSTs the single folder. Unknown GUI fields survive; GUI-only folders
// are never touched.
func (c *HTTPClient) EnsureFolder(ctx context.Context, spec FolderSpec) error {
	if strings.TrimSpace(spec.ID) == "" {
		return fmt.Errorf("syncthing folder id is required")
	}
	merged := map[string]any{}
	if existing, err := c.GetFolder(ctx, spec.ID); err == nil && existing.Raw != nil {
		for k, v := range existing.Raw {
			merged[k] = v
		}
	}
	merged["id"] = spec.ID
	if strings.TrimSpace(spec.Label) != "" {
		merged["label"] = spec.Label
	}
	if strings.TrimSpace(spec.Path) != "" {
		merged["path"] = spec.Path
	}
	if strings.TrimSpace(spec.Type) != "" {
		merged["type"] = spec.Type
	}
	devs := make([]any, 0, len(spec.DeviceIDs))
	for _, id := range spec.DeviceIDs {
		if strings.TrimSpace(id) == "" {
			continue
		}
		devs = append(devs, map[string]any{"deviceID": strings.TrimSpace(id)})
	}
	merged["devices"] = devs
	return c.postJSON(ctx, "/rest/config/folders", merged)
}

// RemoveFolderConfig deletes one folder configuration entry. File bytes on
// disk are preserved; only the configuration entry is removed.
func (c *HTTPClient) RemoveFolderConfig(ctx context.Context, id string) error {
	return c.deleteConfig(ctx, "/rest/config/folders/"+id)
}

// GetDevice reads one device object by device ID.
func (c *HTTPClient) GetDevice(ctx context.Context, id string) (DeviceInfo, error) {
	var raw map[string]any
	if _, err := c.getJSON(ctx, "/rest/config/devices/"+id, &raw); err != nil {
		return DeviceInfo{}, err
	}
	info := DeviceInfo{Raw: raw}
	if did, ok := raw["deviceID"].(string); ok {
		info.ID = strings.TrimSpace(did)
	}
	if name, ok := raw["name"].(string); ok {
		info.Name = name
	}
	return info, nil
}

// ListDevices reads every device object.
func (c *HTTPClient) ListDevices(ctx context.Context) ([]DeviceInfo, error) {
	var raws []map[string]any
	if _, err := c.getJSON(ctx, "/rest/config/devices", &raws); err != nil {
		return nil, err
	}
	out := make([]DeviceInfo, 0, len(raws))
	for _, raw := range raws {
		info := DeviceInfo{Raw: raw}
		if did, ok := raw["deviceID"].(string); ok {
			info.ID = strings.TrimSpace(did)
		}
		if name, ok := raw["name"].(string); ok {
			info.Name = name
		}
		out = append(out, info)
	}
	return out, nil
}

// EnsureDevice merges the managed fields over the existing object (when any)
// and POSTs the single device. Unknown fields survive.
func (c *HTTPClient) EnsureDevice(ctx context.Context, spec DeviceSpec) error {
	if strings.TrimSpace(spec.ID) == "" {
		return fmt.Errorf("syncthing device id is required")
	}
	merged := map[string]any{}
	if existing, err := c.GetDevice(ctx, spec.ID); err == nil && existing.Raw != nil {
		for k, v := range existing.Raw {
			merged[k] = v
		}
	}
	merged["deviceID"] = spec.ID
	if strings.TrimSpace(spec.Name) != "" {
		merged["name"] = spec.Name
	}
	return c.postJSON(ctx, "/rest/config/devices", merged)
}

// RemoveDevice deletes one device configuration entry. Only enrolled-device
// revocations call this; other devices and GUI entries are untouched.
func (c *HTTPClient) RemoveDevice(ctx context.Context, id string) error {
	return c.deleteConfig(ctx, "/rest/config/devices/"+id)
}
