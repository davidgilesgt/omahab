package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/store"
)

const (
	defaultHindsightEmbeddingSocket = "/run/omahab-embedding/embedding.sock"

	hindsightEmbeddingAlias   = "omahab-embed-english"
	hindsightEmbeddingModelID = "nomic-ai/nomic-embed-text-v1.5"
	hindsightEmbeddingDims    = 768

	hindsightEmbeddingSecretScope = "platform-app"
	hindsightEmbeddingSecretName  = "hindsight_embedding_key"

	hindsightEmbeddingUnavailable = "Shared Nomic embedding worker is unavailable"
)

func (b *Backend) embeddingSocketPathOrDefault() string {
	if strings.TrimSpace(b.embeddingSocketPath) != "" {
		return strings.TrimSpace(b.embeddingSocketPath)
	}
	return defaultHindsightEmbeddingSocket
}

func newHindsightEmbeddingClient(socketPath string) *http.Client {
	socketPath = strings.TrimSpace(socketPath)
	if socketPath == "" {
		socketPath = defaultHindsightEmbeddingSocket
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 2 * time.Second}
			return d.DialContext(ctx, "unix", socketPath)
		},
		MaxConnsPerHost: 1,
	}
	return &http.Client{Transport: tr, Timeout: 25 * time.Second}
}

func (b *Backend) embeddingHTTPClientOrDefault() *http.Client {
	if b.embeddingHTTPClient != nil {
		return b.embeddingHTTPClient
	}
	b.embeddingHTTPClient = newHindsightEmbeddingClient(b.embeddingSocketPathOrDefault())
	return b.embeddingHTTPClient
}

// SetHindsightEmbeddingTokenForTest upserts the dedicated embedding token for API contract tests.
func (b *Backend) SetHindsightEmbeddingTokenForTest(ctx context.Context, token string) error {
	if b.secrets == nil {
		return fmt.Errorf("secrets not configured")
	}
	_, err := b.secrets.Put(ctx, hindsightEmbeddingSecretScope, hindsightEmbeddingSecretName, token)
	return err
}

// SetHindsightEmbeddingSocketForTest points the backend at a temporary Unix HTTP test server.
func (b *Backend) SetHindsightEmbeddingSocketForTest(path string) {
	b.embeddingSocketPath = strings.TrimSpace(path)
	b.embeddingHTTPClient = newHindsightEmbeddingClient(b.embeddingSocketPathOrDefault())
}

// ValidateHindsightEmbeddingToken checks the dedicated embedding token.
// Missing secrets or empty tokens return false; secret-store failures return an error.
func (b *Backend) ValidateHindsightEmbeddingToken(ctx context.Context, token string) (bool, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return false, nil
	}
	if b.secrets == nil {
		return false, nil
	}
	stored, err := b.secrets.RevealByName(ctx, hindsightEmbeddingSecretScope, hindsightEmbeddingSecretName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	stored = strings.TrimSpace(stored)
	if stored == "" {
		return false, nil
	}
	got := sha256.Sum256([]byte(token))
	want := sha256.Sum256([]byte(stored))
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		return false, nil
	}
	return true, nil
}

type hindsightEmbedWireRequest struct {
	JobID      string   `json:"job_id"`
	ModelAlias string   `json:"model_alias"`
	Inputs     []string `json:"inputs"`
	Normalize  bool     `json:"normalize"`
}

type hindsightEmbedWireResponse struct {
	JobID      string      `json:"job_id"`
	ModelAlias string      `json:"model_alias"`
	ModelID    string      `json:"model_id"`
	Dimensions int         `json:"dimensions"`
	Vectors    [][]float32 `json:"vectors"`
}

type hindsightHealthModel struct {
	Ready      bool   `json:"ready"`
	ModelID    string `json:"model_id"`
	Dimensions int    `json:"dimensions"`
}

type hindsightHealthResponse struct {
	Models map[string]hindsightHealthModel `json:"models"`
}

// HindsightEmbeddings calls the shared Nomic worker through its Unix socket.
func (b *Backend) HindsightEmbeddings(ctx context.Context, inputs []string) ([][]float32, error) {
	if len(inputs) < 1 || len(inputs) > 16 {
		return nil, fmt.Errorf("%w: inputs must contain 1-16 strings", apitypes.ErrValidation)
	}
	total := 0
	for i, s := range inputs {
		if strings.TrimSpace(s) == "" || s == "" {
			return nil, fmt.Errorf("%w: inputs[%d] must not be empty", apitypes.ErrValidation, i)
		}
		if utf8.RuneCountInString(s) > 50000 {
			return nil, fmt.Errorf("%w: inputs[%d] exceeds 50000 characters", apitypes.ErrValidation, i)
		}
		total += utf8.RuneCountInString(s)
	}
	if total > 131072 {
		return nil, fmt.Errorf("%w: total inputs exceed 131072 characters", apitypes.ErrValidation)
	}
	reqBody := hindsightEmbedWireRequest{
		JobID:      "hindsight-" + store.NewID(),
		ModelAlias: hindsightEmbeddingAlias,
		Inputs:     inputs,
		Normalize:  true,
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", apitypes.ErrServiceUnavailable, hindsightEmbeddingUnavailable)
	}
	client := b.embeddingHTTPClientOrDefault()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://embedding/embed", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", apitypes.ErrServiceUnavailable, hindsightEmbeddingUnavailable)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", apitypes.ErrServiceUnavailable, hindsightEmbeddingUnavailable)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%w: %s", apitypes.ErrServiceUnavailable, hindsightEmbeddingUnavailable)
	}
	limited := io.LimitReader(resp.Body, 1_000_000)
	var wire hindsightEmbedWireResponse
	if err := json.NewDecoder(limited).Decode(&wire); err != nil {
		return nil, fmt.Errorf("%w: %s", apitypes.ErrServiceUnavailable, hindsightEmbeddingUnavailable)
	}
	if strings.TrimSpace(wire.ModelID) != hindsightEmbeddingModelID {
		return nil, fmt.Errorf("%w: %s", apitypes.ErrServiceUnavailable, hindsightEmbeddingUnavailable)
	}
	if wire.Dimensions != hindsightEmbeddingDims {
		return nil, fmt.Errorf("%w: %s", apitypes.ErrServiceUnavailable, hindsightEmbeddingUnavailable)
	}
	if len(wire.Vectors) != len(inputs) {
		return nil, fmt.Errorf("%w: %s", apitypes.ErrServiceUnavailable, hindsightEmbeddingUnavailable)
	}
	for i, vec := range wire.Vectors {
		if len(vec) != hindsightEmbeddingDims {
			return nil, fmt.Errorf("%w: %s", apitypes.ErrServiceUnavailable, hindsightEmbeddingUnavailable)
		}
		for _, v := range vec {
			f := float64(v)
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return nil, fmt.Errorf("%w: %s", apitypes.ErrServiceUnavailable, hindsightEmbeddingUnavailable)
			}
		}
		_ = i
	}
	return wire.Vectors, nil
}

func (b *Backend) checkHindsightEmbeddings(ctx context.Context) error {
	client := b.embeddingHTTPClientOrDefault()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://embedding/health", nil)
	if err != nil {
		return fmt.Errorf("%w: %s", apitypes.ErrServiceUnavailable, hindsightEmbeddingUnavailable)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s", apitypes.ErrServiceUnavailable, hindsightEmbeddingUnavailable)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%w: %s", apitypes.ErrServiceUnavailable, hindsightEmbeddingUnavailable)
	}
	var health hindsightHealthResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1_000_000)).Decode(&health); err != nil {
		return fmt.Errorf("%w: %s", apitypes.ErrServiceUnavailable, hindsightEmbeddingUnavailable)
	}
	entry, ok := health.Models[hindsightEmbeddingAlias]
	if !ok || !entry.Ready {
		return fmt.Errorf("%w: %s", apitypes.ErrServiceUnavailable, hindsightEmbeddingUnavailable)
	}
	if strings.TrimSpace(entry.ModelID) != hindsightEmbeddingModelID || entry.Dimensions != hindsightEmbeddingDims {
		return fmt.Errorf("%w: %s", apitypes.ErrServiceUnavailable, hindsightEmbeddingUnavailable)
	}
	if _, err := b.HindsightEmbeddings(ctx, []string{"search_query: Hindsight readiness"}); err != nil {
		return err
	}
	return nil
}
