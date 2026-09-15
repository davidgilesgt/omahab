package client

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type localStub struct {
	folders map[string]map[string]any
	paused  []string
	deleted []string
	posted  []string
}

func (s *localStub) handler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == "GET" && path == "/rest/config/folders":
		var out []any
		for _, f := range s.folders {
			out = append(out, f)
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == "GET" && strings.HasPrefix(path, "/rest/config/folders/"):
		id := strings.TrimPrefix(path, "/rest/config/folders/")
		if f, ok := s.folders[id]; ok {
			_ = json.NewEncoder(w).Encode(f)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	case r.Method == "POST" && path == "/rest/config/folders":
		body, _ := io.ReadAll(r.Body)
		var obj map[string]any
		_ = json.Unmarshal(body, &obj)
		id, _ := obj["id"].(string)
		s.folders[id] = obj
		s.posted = append(s.posted, id)
		if _, ok := obj["paused"]; ok {
			s.paused = append(s.paused, id)
		}
		w.WriteHeader(http.StatusOK)
	case r.Method == "DELETE" && strings.HasPrefix(path, "/rest/config/folders/"):
		id := strings.TrimPrefix(path, "/rest/config/folders/")
		delete(s.folders, id)
		s.deleted = append(s.deleted, id)
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// Canonical wire IDs are used verbatim, never re-derived from names.
func TestEnsureLocalFolderUsesWireID(t *testing.T) {
	stub := &localStub{folders: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()
	syncthingLocalBase = srv.URL
	t.Cleanup(func() { syncthingLocalBase = "http://127.0.0.1:8384" })
	dir := t.TempDir()
	local := filepath.Join(dir, "vault")
	if err := ensureLocalSyncthingFolder("k", "server-wire-id", "My Vault", local, "DEV1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := stub.folders["server-wire-id"]; !ok {
		t.Fatalf("folders = %v, want canonical wire ID", stub.folders)
	}
	if _, ok := stub.folders["my-vault"]; ok {
		t.Fatal("name-derived IDs must not be created")
	}
}

// Stale same-path entries are paused, preserved and replaced canonically.
func TestEnsureLocalFolderRetiresStaleID(t *testing.T) {
	stub := &localStub{folders: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()
	syncthingLocalBase = srv.URL
	t.Cleanup(func() { syncthingLocalBase = "http://127.0.0.1:8384" })
	dir := t.TempDir()
	local := filepath.Join(dir, "vault")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(local, "note.md")
	if err := os.WriteFile(keep, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	stub.folders["old-managed"] = map[string]any{"id": "old-managed", "label": "Vault", "path": local, "type": "sendreceive"}
	if err := ensureLocalSyncthingFolder("k", "server-wire-id", "Vault", local, "DEV1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := stub.folders["old-managed"]; ok {
		t.Fatal("old configuration entry must be removed")
	}
	if _, ok := stub.folders["server-wire-id"]; !ok {
		t.Fatal("canonical entry must be recreated (expect a rescan)")
	}
	foundPaused := false
	for _, id := range stub.paused {
		if id == "old-managed" {
			foundPaused = true
		}
	}
	if !foundPaused {
		t.Fatal("old folder must be paused before removal")
	}
	if got, _ := os.ReadFile(keep); string(got) != "bytes" {
		t.Fatal("file bytes must be preserved; no data deletion")
	}
}
