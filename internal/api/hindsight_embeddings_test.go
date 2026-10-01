package api

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type embeddingFakeWorker struct {
	mu       sync.Mutex
	mode     string // "", "wrong-dims", "failure", "wrong-model"
	received [][]string
}

func (f *embeddingFakeWorker) setMode(m string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode = m
}

func (f *embeddingFakeWorker) getMode() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mode
}

func (f *embeddingFakeWorker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && (r.URL.Path == "/health" || r.URL.Path == "/up"):
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","models":{"omahab-embed-english":{"ready":true,"model_id":"nomic-ai/nomic-embed-text-v1.5","dimensions":768,"artifact_path":"test","expected_memory_mb":null}}}`))
		return
	case r.Method == http.MethodPost && (r.URL.Path == "/embed" || r.URL.Path == "/v1/embed"):
		mode := f.getMode()
		var req struct {
			JobID      string   `json:"job_id"`
			ModelAlias string   `json:"model_alias"`
			Inputs     []string `json:"inputs"`
			Normalize  bool     `json:"normalize"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1_000_000)).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.received = append(f.received, append([]string{}, req.Inputs...))
		f.mu.Unlock()
		if mode == "failure" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"inference_failed","message":"boom"}}`))
			return
		}
		modelID := "nomic-ai/nomic-embed-text-v1.5"
		dims := 768
		if mode == "wrong-model" {
			modelID = "other/model"
		}
		if mode == "wrong-dims" {
			dims = 1024
		}
		vecCount := dims
		if mode == "wrong-dims" {
			// Return 1024-length vectors to trigger dimension mismatch.
		} else {
			vecCount = 768
		}
		vectors := make([][]float32, 0, len(req.Inputs))
		for i := range req.Inputs {
			vec := make([]float32, vecCount)
			base := float32(i+1) * 0.1
			for k := range vec {
				vec[k] = base + float32(k)*0.0001
			}
			vectors = append(vectors, vec)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"job_id":      req.JobID,
			"model_alias": req.ModelAlias,
			"model_id":    modelID,
			"dimensions":  dims,
			"vectors":     vectors,
			"checksums":   []string{},
			"usage":       map[string]any{"input_count": len(req.Inputs)},
		})
		return
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func startEmbeddingFake(t *testing.T, fake *embeddingFakeWorker) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "embedding-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "embedding.sock")
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &http.Server{Handler: fake}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = os.Remove(sock)
	})
	return sock
}

func newHindsightTestSetup(t *testing.T, fake *embeddingFakeWorker) (*Server, string) {
	t.Helper()
	backend := newRealBackend(t, nil)
	sock := startEmbeddingFake(t, fake)
	backend.SetHindsightEmbeddingSocketForTest(sock)
	const token = "test-hindsight-embedding-token-123"
	if err := backend.SetHindsightEmbeddingTokenForTest(context.Background(), token); err != nil {
		t.Fatalf("set embedding token: %v", err)
	}
	srv := newRealServer(t, backend)
	return srv, token
}

func doHindsightPost(t *testing.T, srv *Server, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hindsight/embeddings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHindsightEmbeddings_AuthRejects(t *testing.T) {
	fake := &embeddingFakeWorker{}
	srv, token := newHindsightTestSetup(t, fake)
	validBody := `{"model":"omahab-embed-english","input":"search_query: hello"}`

	if rec := doHindsightPost(t, srv, "", validBody); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token = %d, want 401, body=%s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hindsight/embeddings", strings.NewReader(validBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Token "+token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("malformed auth = %d, want 401, body=%s", rec.Code, rec.Body.String())
	}
	if rec := doHindsightPost(t, srv, "wrong-token", validBody); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong token = %d, want 403, body=%s", rec.Code, rec.Body.String())
	}
	if rec := doHindsightPost(t, srv, "test-token", validBody); rec.Code != http.StatusForbidden {
		t.Fatalf("admin token = %d, want 403, body=%s", rec.Code, rec.Body.String())
	}
	if rec := doHindsightPost(t, srv, "oma_dev_abc123", validBody); rec.Code != http.StatusForbidden {
		t.Fatalf("device token = %d, want 403, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHindsightEmbeddings_ValidFloatAndBase64(t *testing.T) {
	fake := &embeddingFakeWorker{}
	srv, token := newHindsightTestSetup(t, fake)
	body := `{"model":"omahab-embed-english","input":["search_document: My train seat is 12A.","search_query: Which train seat did I choose?"]}`

	floatRec := doHindsightPost(t, srv, token, body)
	if floatRec.Code != http.StatusOK {
		t.Fatalf("float status = %d, body=%s", floatRec.Code, floatRec.Body.String())
	}
	if got := floatRec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var floatResp struct {
		Object string `json:"object"`
		Model  string `json:"model"`
		Data   []struct {
			Object    string    `json:"object"`
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(floatRec.Body.Bytes(), &floatResp); err != nil {
		t.Fatalf("decode float: %v", err)
	}
	if floatResp.Object != "list" || floatResp.Model != "omahab-embed-english" || len(floatResp.Data) != 2 {
		t.Fatalf("float resp = %+v", floatResp)
	}
	for i, d := range floatResp.Data {
		if d.Object != "embedding" || d.Index != i || len(d.Embedding) != 768 {
			t.Fatalf("float entry %d = %+v", i, d)
		}
	}

	b64Body := `{"model":"omahab-embed-english","input":["search_document: My train seat is 12A.","search_query: Which train seat did I choose?"],"encoding_format":"base64"}`
	b64Rec := doHindsightPost(t, srv, token, b64Body)
	if b64Rec.Code != http.StatusOK {
		t.Fatalf("base64 status = %d, body=%s", b64Rec.Code, b64Rec.Body.String())
	}
	var b64Resp struct {
		Object string `json:"object"`
		Model  string `json:"model"`
		Data   []struct {
			Object    string `json:"object"`
			Index     int    `json:"index"`
			Embedding string `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b64Rec.Body.Bytes(), &b64Resp); err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	if len(b64Resp.Data) != 2 {
		t.Fatalf("base64 entries = %d", len(b64Resp.Data))
	}
	for i, d := range b64Resp.Data {
		raw, err := base64.StdEncoding.DecodeString(d.Embedding)
		if err != nil {
			t.Fatalf("base64 decode %d: %v", i, err)
		}
		if len(raw) != 768*4 {
			t.Fatalf("base64 bytes %d = %d, want %d", i, len(raw), 768*4)
		}
		for k := 0; k < 768; k++ {
			got := math.Float32frombits(binary.LittleEndian.Uint32(raw[k*4:]))
			want := floatResp.Data[i].Embedding[k]
			if diff := math.Abs(float64(got) - float64(want)); diff > 0.000002 {
				t.Fatalf("entry %d dim %d diff %v (got %v want %v)", i, k, diff, got, want)
			}
		}
	}
	if strings.Contains(floatRec.Body.String(), "usage") {
		t.Fatalf("response must omit usage: %s", floatRec.Body.String())
	}
}

func TestHindsightEmbeddings_RejectsBadRequests(t *testing.T) {
	fake := &embeddingFakeWorker{}
	srv, token := newHindsightTestSetup(t, fake)
	cases := map[string]string{
		"wrong model":   `{"model":"omahab-embed-worldwide","input":"hi"}`,
		"null input":    `{"model":"omahab-embed-english","input":null}`,
		"token ids":     `{"model":"omahab-embed-english","input":[1,2,3]}`,
		"bad dims":      `{"model":"omahab-embed-english","input":"hi","dimensions":1024}`,
		"bad format":    `{"model":"omahab-embed-english","input":"hi","encoding_format":"hex"}`,
		"unknown field": `{"model":"omahab-embed-english","input":"hi","foo":1}`,
		"empty string":  `{"model":"omahab-embed-english","input":""}`,
		"empty array":   `{"model":"omahab-embed-english","input":[]}`,
		"too many":      `{"model":"omahab-embed-english","input":["a","a","a","a","a","a","a","a","a","a","a","a","a","a","a","a","a"]}`,
		"missing model": `{"input":"hi"}`,
	}
	for name, body := range cases {
		if rec := doHindsightPost(t, srv, token, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400, body=%s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestHindsightEmbeddings_Rotation(t *testing.T) {
	fake := &embeddingFakeWorker{}
	backend := newRealBackend(t, nil)
	sock := startEmbeddingFake(t, fake)
	backend.SetHindsightEmbeddingSocketForTest(sock)
	const oldToken = "old-embedding-token-1"
	const newToken = "new-embedding-token-2"
	if err := backend.SetHindsightEmbeddingTokenForTest(context.Background(), oldToken); err != nil {
		t.Fatalf("set old: %v", err)
	}
	srv := newRealServer(t, backend)
	body := `{"model":"omahab-embed-english","input":"hello"}`
	if rec := doHindsightPost(t, srv, oldToken, body); rec.Code != http.StatusOK {
		t.Fatalf("old token before rotation = %d, want 200", rec.Code)
	}
	if err := backend.SetHindsightEmbeddingTokenForTest(context.Background(), newToken); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rec := doHindsightPost(t, srv, oldToken, body); rec.Code != http.StatusForbidden {
		t.Fatalf("old token after rotation = %d, want 403", rec.Code)
	}
	if rec := doHindsightPost(t, srv, newToken, body); rec.Code != http.StatusOK {
		t.Fatalf("new token after rotation = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHindsightEmbeddings_WorkerFailuresAre503(t *testing.T) {
	fake := &embeddingFakeWorker{}
	srv, token := newHindsightTestSetup(t, fake)
	body := `{"model":"omahab-embed-english","input":"secret train seat 12A"}`

	fake.setMode("failure")
	rec := doHindsightPost(t, srv, token, body)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("failure mode = %d, want 503, body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret train seat") || strings.Contains(rec.Body.String(), "vectors") {
		t.Fatalf("503 must not leak input or vectors: %s", rec.Body.String())
	}

	fake.setMode("wrong-dims")
	rec = doHindsightPost(t, srv, token, body)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("wrong dims = %d, want 503, body=%s", rec.Code, rec.Body.String())
	}

	fake.setMode("wrong-model")
	rec = doHindsightPost(t, srv, token, body)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("wrong model = %d, want 503, body=%s", rec.Code, rec.Body.String())
	}
}
