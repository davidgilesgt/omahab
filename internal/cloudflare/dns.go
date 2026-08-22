package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	cloudflare "github.com/cloudflare/cloudflare-go/v2"
	"github.com/omahab/omahab/internal/exposure"
	"github.com/omahab/omahab/internal/store"
)

type dnsClient struct {
	zoneID string
	client *cloudflare.Client
}

func newDNSClient(c *cloudflare.Client, zoneID string) *dnsClient {
	return &dnsClient{zoneID: zoneID, client: c}
}

func (c *dnsClient) ListRecords(ctx context.Context) ([]exposure.Record, error) {
	var env struct {
		Result []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Type    string `json:"type"`
			Content string `json:"content"`
			Proxied bool   `json:"proxied"`
			TTL     int    `json:"ttl"`
		} `json:"result"`
		Success bool `json:"success"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	path := fmt.Sprintf("zones/%s/dns_records", c.zoneID)
	if err := c.client.Execute(ctx, http.MethodGet, path, nil, &env); err != nil {
		return nil, mapAPIError(err)
	}
	out := make([]exposure.Record, 0, len(env.Result))
	for _, r := range env.Result {
		out = append(out, exposure.Record{
			ID:      r.ID,
			Name:    r.Name,
			Type:    r.Type,
			Content: r.Content,
			Proxied: r.Proxied,
			TTL:     r.TTL,
		})
	}
	return out, nil
}

func (c *dnsClient) CreateRecord(ctx context.Context, rec exposure.Record) (string, error) {
	if rec.Name == "" || rec.Type == "" || rec.Content == "" {
		return "", fmt.Errorf("%w: name, type and content are required", store.ErrValidation)
	}
	if rec.Type != "A" && rec.Type != "CNAME" {
		return "", fmt.Errorf("%w: unsupported record type %q", store.ErrValidation, rec.Type)
	}
	body := map[string]any{
		"type":    rec.Type,
		"name":    rec.Name,
		"content": rec.Content,
		"ttl":     rec.TTL,
		"proxied": rec.Proxied,
	}
	var env struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
		Success bool `json:"success"`
	}
	path := fmt.Sprintf("zones/%s/dns_records", c.zoneID)
	if err := c.client.Execute(ctx, http.MethodPost, path, body, &env); err != nil {
		return "", mapAPIError(err)
	}
	if env.Result.ID == "" {
		return "", fmt.Errorf("cloudflare: create record returned empty id")
	}
	return env.Result.ID, nil
}

func (c *dnsClient) ReplaceRecord(ctx context.Context, id string, rec exposure.Record) error {
	if id == "" {
		return fmt.Errorf("%w: record id is required", store.ErrValidation)
	}
	if rec.Name == "" || rec.Type == "" || rec.Content == "" {
		return fmt.Errorf("%w: name, type and content are required", store.ErrValidation)
	}
	body := map[string]any{
		"type":    rec.Type,
		"name":    rec.Name,
		"content": rec.Content,
		"ttl":     rec.TTL,
		"proxied": rec.Proxied,
	}
	var env struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
		Success bool `json:"success"`
	}
	path := fmt.Sprintf("zones/%s/dns_records/%s", c.zoneID, id)
	if err := c.client.Execute(ctx, http.MethodPut, path, body, &env); err != nil {
		return mapAPIError(err)
	}
	return nil
}

func (c *dnsClient) DeleteRecord(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("%w: record id is required", store.ErrValidation)
	}
	var env struct {
		Success bool `json:"success"`
	}
	path := fmt.Sprintf("zones/%s/dns_records/%s", c.zoneID, id)
	if err := c.client.Execute(ctx, http.MethodDelete, path, nil, &env); err != nil {
		mapped := mapAPIError(err)
		if errors.Is(mapped, store.ErrNotFound) {
			return nil
		}
		return mapped
	}
	return nil
}
