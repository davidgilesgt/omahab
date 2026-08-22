package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HTTPClient is a minimal Syncthing REST client. It implements SyncthingClient
// and converts SDK/HTTP types at the edge.
type HTTPClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewHTTPClient creates a Syncthing HTTP client. baseURL is the Syncthing
// REST base, e.g. http://127.0.0.1:8384. apiKey is sent as X-API-Key.
func NewHTTPClient(baseURL, apiKey string) *HTTPClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	return &HTTPClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 5 * time.Second},
	}
}

// NewHTTPClientWithHTTP is like NewHTTPClient but allows injecting http.Client for tests.
func NewHTTPClientWithHTTP(baseURL, apiKey string, hc *http.Client) *HTTPClient {
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	return &HTTPClient{baseURL: baseURL, apiKey: apiKey, http: hc}
}

func (c *HTTPClient) request(ctx context.Context, method, path string, query url.Values) (*http.Request, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("syncthing base URL not configured")
	}
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// FolderErrors returns the folder error string via Syncthing's REST API.
// It queries /rest/folder/errors and /rest/db/status; a non-empty string
// indicates the folder needs attention.
func (c *HTTPClient) FolderErrors(ctx context.Context, folder string) (string, error) {
	folder = strings.TrimSpace(folder)
	if folder == "" {
		return "", nil
	}
	// Try /rest/folder/errors?folder=<id>
	q := url.Values{}
	q.Set("folder", folder)
	req, err := c.request(ctx, http.MethodGet, "/rest/folder/errors", q)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// No such folder in Syncthing yet; not an error for syncer.
		return "", nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("syncthing folder errors status %d", resp.StatusCode)
	}
	var payload struct {
		Errors []struct {
			Path  string `json:"path"`
			Error string `json:"error"`
		} `json:"errors"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", nil
	}
	if strings.TrimSpace(payload.Error) != "" {
		return strings.TrimSpace(payload.Error), nil
	}
	if len(payload.Errors) > 0 {
		var parts []string
		for _, e := range payload.Errors {
			if strings.TrimSpace(e.Error) != "" {
				if e.Path != "" {
					parts = append(parts, e.Path+": "+e.Error)
				} else {
					parts = append(parts, e.Error)
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "; "), nil
		}
	}
	// Fallback: check /rest/db/status?folder=<id> for errors field
	q2 := url.Values{}
	q2.Set("folder", folder)
	req2, err := c.request(ctx, http.MethodGet, "/rest/db/status", q2)
	if err != nil {
		return "", nil
	}
	resp2, err := c.http.Do(req2)
	if err != nil {
		return "", nil
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		return "", nil
	}
	var status struct {
		State string `json:"state"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&status); err != nil {
		return "", nil
	}
	if strings.TrimSpace(status.Error) != "" {
		return strings.TrimSpace(status.Error), nil
	}
	if status.State == "error" || status.State == "failed" {
		return "folder state: " + status.State, nil
	}
	return "", nil
}

// Connections returns device last-seen info. It merges /rest/stats/device
// (lastSeen) and /rest/system/connections (connected).
func (c *HTTPClient) Connections(ctx context.Context) (map[string]ConnectionInfo, error) {
	out := make(map[string]ConnectionInfo)

	// /rest/stats/device -> {"DEVICEID": {"lastSeen": "2020-...", ...}}
	req, err := c.request(ctx, http.MethodGet, "/rest/stats/device", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		var stats map[string]struct {
			LastSeen string `json:"lastSeen"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&stats); err == nil {
			for id, st := range stats {
				var t time.Time
				if st.LastSeen != "" {
					// Syncthing emits RFC3339 nano.
					t, _ = time.Parse(time.RFC3339Nano, st.LastSeen)
					if t.IsZero() {
						t, _ = time.Parse(time.RFC3339, st.LastSeen)
					}
				}
				ci := out[id]
				ci.LastSeen = t
				out[id] = ci
			}
		}
	}

	// /rest/system/connections -> {"connections": {"DEVICEID": {"connected": true, ...}}}
	req2, err := c.request(ctx, http.MethodGet, "/rest/system/connections", nil)
	if err != nil {
		// Return what we have from stats if connections fails.
		return out, nil
	}
	resp2, err := c.http.Do(req2)
	if err != nil {
		return out, nil
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		return out, nil
	}
	var conns struct {
		Connections map[string]struct {
			Connected bool   `json:"connected"`
			Paused    bool   `json:"paused"`
			Address   string `json:"address"`
		} `json:"connections"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&conns); err != nil {
		return out, nil
	}
	for id, cc := range conns.Connections {
		ci := out[id]
		ci.Connected = cc.Connected
		out[id] = ci
	}
	return out, nil
}
