package syncer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type stubSyncthing struct {
	t       *testing.T
	folders map[string]map[string]any
	devices map[string]map[string]any
	posts   []string
	deletes []string
	bodies  map[string][]byte
}

func newStubSyncthing(t *testing.T) *stubSyncthing {
	return &stubSyncthing{t: t, folders: map[string]map[string]any{}, devices: map[string]map[string]any{}, bodies: map[string][]byte{}}
}

func (s *stubSyncthing) handler(w http.ResponseWriter, r *http.Request) {
	if key := r.Header.Get("X-API-Key"); key != "k" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
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
		if err := json.Unmarshal(body, &obj); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		id, _ := obj["id"].(string)
		s.folders[id] = obj
		s.posts = append(s.posts, "folders:"+id)
		s.bodies["folders:"+id] = body
		w.WriteHeader(http.StatusOK)
	case r.Method == "DELETE" && strings.HasPrefix(path, "/rest/config/folders/"):
		id := strings.TrimPrefix(path, "/rest/config/folders/")
		delete(s.folders, id)
		s.deletes = append(s.deletes, "folders:"+id)
		w.WriteHeader(http.StatusOK)
	case r.Method == "GET" && strings.HasPrefix(path, "/rest/config/devices/"):
		id := strings.TrimPrefix(path, "/rest/config/devices/")
		if d, ok := s.devices[id]; ok {
			_ = json.NewEncoder(w).Encode(d)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	case r.Method == "POST" && path == "/rest/config/devices":
		body, _ := io.ReadAll(r.Body)
		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		id, _ := obj["deviceID"].(string)
		s.devices[id] = obj
		s.posts = append(s.posts, "devices:"+id)
		w.WriteHeader(http.StatusOK)
	case r.Method == "DELETE" && strings.HasPrefix(path, "/rest/config/devices/"):
		id := strings.TrimPrefix(path, "/rest/config/devices/")
		delete(s.devices, id)
		s.deletes = append(s.deletes, "devices:"+id)
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// EnsureFolder POSTs one object and preserves unknown GUI fields.
func TestEnsureFolderMerges(t *testing.T) {
	stub := newStubSyncthing(t)
	stub.folders["wid"] = map[string]any{
		"id": "wid", "label": "Old", "path": "/x", "type": "sendreceive",
		"guiCustom": "keep-me",
		"devices":   []any{map[string]any{"deviceID": "OTHER"}},
	}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()
	c := NewHTTPClientWithHTTP(srv.URL, "k", srv.Client())
	err := c.EnsureFolder(context.Background(), FolderSpec{
		ID: "wid", Label: "New", Path: "/srv/obsidian", Type: "sendreceive", DeviceIDs: []string{"D1", "D2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := stub.folders["wid"]
	if got["guiCustom"] != "keep-me" {
		t.Fatalf("unknown fields must be preserved: %v", got)
	}
	if got["label"] != "New" || got["path"] != "/srv/obsidian" {
		t.Fatalf("managed fields = %v", got)
	}
	devs, _ := got["devices"].([]any)
	if len(devs) != 2 {
		t.Fatalf("sharing must be exactly enrolled devices: %v", got["devices"])
	}
	if len(stub.posts) != 1 || stub.posts[0] != "folders:wid" {
		t.Fatalf("must POST one object, got %v", stub.posts)
	}
}

// EnsureDevice preserves unknown fields; removal is narrowly owned.
func TestEnsureAndRemoveDevice(t *testing.T) {
	stub := newStubSyncthing(t)
	stub.devices["D1"] = map[string]any{"deviceID": "D1", "name": "old", "custom": "keep"}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()
	c := NewHTTPClientWithHTTP(srv.URL, "k", srv.Client())
	if err := c.EnsureDevice(context.Background(), DeviceSpec{ID: "D1", Name: "laptop"}); err != nil {
		t.Fatal(err)
	}
	if stub.devices["D1"]["custom"] != "keep" || stub.devices["D1"]["name"] != "laptop" {
		t.Fatalf("device = %v", stub.devices["D1"])
	}
	if err := c.EnsureDevice(context.Background(), DeviceSpec{ID: "D2", Name: "phone"}); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveDevice(context.Background(), "D2"); err != nil {
		t.Fatal(err)
	}
	if _, ok := stub.devices["D2"]; ok {
		t.Fatal("revoked device entry must be removed")
	}
	if _, ok := stub.devices["D1"]; !ok {
		t.Fatal("other devices must stay")
	}
}

// Auth failures surface distinctly.
func TestConfigAuthError(t *testing.T) {
	stub := newStubSyncthing(t)
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()
	c := NewHTTPClientWithHTTP(srv.URL, "wrong", srv.Client())
	err := c.EnsureFolder(context.Background(), FolderSpec{ID: "x", Path: "/x"})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want 403", err)
	}
}
