package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/omahab/omahab/internal/exposure"
	"github.com/omahab/omahab/internal/store"
)

type edgeClient struct {
	baseURL    string
	httpClient *http.Client
}

func newEdgeClient(baseURL string, httpClient *http.Client) *edgeClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	return &edgeClient{baseURL: baseURL, httpClient: httpClient}
}

func (c *edgeClient) do(ctx context.Context, method, path string, body any, out any) error {
	url := c.baseURL + path
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mapHTTPStatus(resp.StatusCode, string(b))
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("edge: decode response: %w", err)
		}
	}
	return nil
}
func (c *edgeClient) ListRoutes(ctx context.Context) ([]exposure.Route, error) {
	var out []exposure.Route
	if err := c.do(ctx, http.MethodGet, "/routes", nil, &out); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return []exposure.Route{}, nil
		}
		return nil, err
	}
	if out == nil {
		out = []exposure.Route{}
	}
	return out, nil
}

func (c *edgeClient) PutRoute(ctx context.Context, route exposure.Route) error {
	if strings.TrimSpace(route.Hostname) == "" || strings.TrimSpace(route.Upstream) == "" {
		return fmt.Errorf("%w: hostname and upstream are required", store.ErrValidation)
	}
	path := fmt.Sprintf("/routes/%s", route.Hostname)
	return c.do(ctx, http.MethodPut, path, route, nil)
}

func (c *edgeClient) DeleteRoute(ctx context.Context, hostname string) error {
	if strings.TrimSpace(hostname) == "" {
		return fmt.Errorf("%w: hostname is required", store.ErrValidation)
	}
	path := fmt.Sprintf("/routes/%s", hostname)
	if err := c.do(ctx, http.MethodDelete, path, nil, nil); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	return nil
}
