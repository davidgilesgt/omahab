package scm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newWoodpeckerTestClient(srv *httptest.Server, token string) WoodpeckerClient {
	return NewWoodpeckerClient(WoodpeckerConfig{
		BaseURL:    srv.URL,
		Token:      token,
		HTTPClient: srv.Client(),
	})
}

func TestWoodpeckerEnsureRepo(t *testing.T) {
	tests := []struct {
		name    string
		token   string
		in      EnsureCIRepoInput
		handler func(w http.ResponseWriter, r *http.Request)
		wantErr error
	}{
		{
			name:  "success",
			token: "tok",
			in:    EnsureCIRepoInput{ForgejoRemoteID: 42, Owner: "alice", Name: "myrepo", PipelinePath: ".woodpecker.yaml"},
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/repos" || r.URL.Query().Get("forge_remote_id") != "42" {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				if r.Header.Get("Authorization") != "Bearer tok" {
					http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": 10, "forge_remote_id": "42", "owner": "alice", "name": "myrepo",
					"full_name": "alice/myrepo", "active": true, "config_file": ".woodpecker.yaml",
				})
			},
		},
		{
			name:  "with patch for trusted",
			token: "tok",
			in:    EnsureCIRepoInput{ForgejoRemoteID: 42, Owner: "alice", Name: "myrepo", Trusted: true, PipelinePath: "ci/woodpecker.yaml"},
			handler: func() func(w http.ResponseWriter, r *http.Request) {
				patched := false
				return func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost && r.URL.Path == "/api/repos" {
						_ = json.NewEncoder(w).Encode(map[string]any{
							"id": 10, "forge_remote_id": "42", "owner": "alice", "name": "myrepo", "active": true,
						})
						return
					}
					if r.Method == http.MethodPatch && r.URL.Path == "/api/repos/10" {
						var body map[string]any
						_ = json.NewDecoder(r.Body).Decode(&body)
						if body["config_file"] != "ci/woodpecker.yaml" {
							http.Error(w, `{"message":"bad config"}`, http.StatusBadRequest)
							return
						}
						patched = true
						_ = json.NewEncoder(w).Encode(map[string]any{
							"id": 10, "forge_remote_id": "42", "owner": "alice", "name": "myrepo", "active": true,
							"config_file": "ci/woodpecker.yaml",
							"trusted": map[string]bool{"network": true, "volumes": true, "security": true},
						})
						return
					}
					if patched {
						return
					}
					http.Error(w, "not found", http.StatusNotFound)
				}
			}(),
		},
		{
			name:  "auth failure",
			token: "bad",
			in:    EnsureCIRepoInput{ForgejoRemoteID: 42, Owner: "alice", Name: "myrepo"},
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer tok" {
					http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
					return
				}
				w.WriteHeader(http.StatusOK)
			},
			wantErr: ErrValidation,
		},
		{
			name:  "conflict already active",
			token: "tok",
			in:    EnsureCIRepoInput{ForgejoRemoteID: 42, Owner: "alice", Name: "myrepo"},
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"message":"already active"}`, http.StatusConflict)
			},
			wantErr: ErrConflict,
		},
		{
			name:    "validation missing remote id",
			token:   "tok",
			in:      EnsureCIRepoInput{Owner: "alice", Name: "myrepo"},
			handler: func(w http.ResponseWriter, r *http.Request) { t.Fatal("should not hit server") },
			wantErr: ErrValidation,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(tc.handler))
			defer srv.Close()
			c := newWoodpeckerTestClient(srv, tc.token)
			got, err := c.EnsureRepo(context.Background(), tc.in)
			if tc.wantErr != nil {
				if err == nil || !errors.Is(err, tc.wantErr) {
					t.Fatalf("err %v want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("EnsureRepo: %v", err)
			}
			if got.ID == 0 {
				t.Fatal("expected id")
			}
			if got.Owner != tc.in.Owner || got.Name != tc.in.Name {
				t.Fatalf("got %s/%s want %s/%s", got.Owner, got.Name, tc.in.Owner, tc.in.Name)
			}
		})
	}
}

func TestWoodpeckerDeactivateRepo(t *testing.T) {
	tests := []struct {
		name    string
		id      int64
		code    int
		wantErr error
	}{
		{"success", 10, http.StatusOK, nil},
		{"not found", 99, http.StatusNotFound, ErrNotFound},
		{"auth", 10, http.StatusForbidden, ErrValidation},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete || r.URL.Path != "/api/repos/10" && tc.name == "success" {
					// allow path check per case
				}
				if tc.name == "not found" {
					http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
					return
				}
				if tc.name == "auth" {
					http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
					return
				}
				w.WriteHeader(tc.code)
			}))
			defer srv.Close()
			c := newWoodpeckerTestClient(srv, "tok")
			err := c.DeactivateRepo(context.Background(), tc.id)
			if tc.wantErr != nil {
				if err == nil || !errors.Is(err, tc.wantErr) {
					t.Fatalf("err %v want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("DeactivateRepo: %v", err)
			}
		})
	}
}

func TestWoodpeckerListRuns(t *testing.T) {
	pipelines := []map[string]any{
		{"id": 100, "number": 2, "status": "success", "branch": "main", "commit": "abc", "event": "push", "message": "msg2", "author": "alice", "started": int64(1000), "finished": int64(2000)},
		{"id": 101, "number": 1, "status": "failure", "branch": "main", "commit": "def", "event": "push", "message": "msg1", "author": "bob"},
	}
	tests := []struct {
		name    string
		repoID  int64
		limit   int
		handler func(w http.ResponseWriter, r *http.Request)
		wantN   int
		wantErr error
	}{
		{
			name:   "success",
			repoID: 10,
			limit:  10,
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/repos/10/pipelines" {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				if r.URL.Query().Get("perPage") != "10" {
					http.Error(w, `{"message":"bad limit"}`, http.StatusBadRequest)
					return
				}
				_ = json.NewEncoder(w).Encode(pipelines)
			},
			wantN: 2,
		},
		{
			name:   "no limit",
			repoID: 10,
			handler: func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(pipelines)
			},
			wantN: 2,
		},
		{
			name:   "unauthorized",
			repoID: 10,
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			},
			wantErr: ErrValidation,
		},
		{
			name:   "not found",
			repoID: 99,
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"message":"repo not found"}`, http.StatusNotFound)
			},
			wantErr: ErrNotFound,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(tc.handler))
			defer srv.Close()
			c := newWoodpeckerTestClient(srv, "tok")
			got, err := c.ListRuns(context.Background(), tc.repoID, tc.limit)
			if tc.wantErr != nil {
				if err == nil || !errors.Is(err, tc.wantErr) {
					t.Fatalf("err %v want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ListRuns: %v", err)
			}
			if len(got) != tc.wantN {
				t.Fatalf("got %d want %d", len(got), tc.wantN)
			}
			if got[0].Status == "" || got[0].CommitSHA == "" {
				t.Fatal("missing fields")
			}
		})
	}
}

func TestWoodpeckerGetRun(t *testing.T) {
	tests := []struct {
		name    string
		repoID  int64
		number  int
		code    int
		body    any
		wantErr error
	}{
		{"success", 10, 5, http.StatusOK, map[string]any{"id": 200, "number": 5, "status": "success", "branch": "main", "commit": "abc123", "event": "push", "message": "hi", "author": "alice", "started": int64(100), "finished": int64(200)}, nil},
		{"not found", 10, 99, http.StatusNotFound, map[string]any{"message": "not found"}, ErrNotFound},
		{"unauthorized", 10, 5, http.StatusUnauthorized, map[string]any{"message": "unauthorized"}, ErrValidation},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/repos/10/pipelines/5" && tc.name == "success" {
					// still respond with tc.code for error cases
				}
				if tc.name == "not found" && r.URL.Path == "/api/repos/10/pipelines/99" {
					http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
					return
				}
				w.WriteHeader(tc.code)
				_ = json.NewEncoder(w).Encode(tc.body)
			}))
			defer srv.Close()
			c := newWoodpeckerTestClient(srv, "tok")
			got, err := c.GetRun(context.Background(), tc.repoID, tc.number)
			if tc.wantErr != nil {
				if err == nil || !errors.Is(err, tc.wantErr) {
					t.Fatalf("err %v want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetRun: %v", err)
			}
			if got.Number != tc.number {
				t.Fatalf("number %d != %d", got.Number, tc.number)
			}
		})
	}
	// validation
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("should not hit server") }))
	defer srv.Close()
	c := newWoodpeckerTestClient(srv, "tok")
	if _, err := c.GetRun(context.Background(), 0, 1); !errors.Is(err, ErrValidation) {
		t.Fatalf("want validation got %v", err)
	}
	if _, err := c.GetRun(context.Background(), 10, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("want validation got %v", err)
	}
}

func TestWoodpeckerLogRefs(t *testing.T) {
	pipelineJSON := map[string]any{
		"id": 200, "number": 5, "status": "success", "branch": "main",
		"workflows": []any{
			map[string]any{
				"name": "default",
				"children": []any{
					map[string]any{"id": 300, "name": "clone", "pid": 1},
					map[string]any{"id": 301, "name": "build", "pid": 2},
				},
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/repos/10/pipelines/5" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(pipelineJSON)
	}))
	defer srv.Close()
	c := newWoodpeckerTestClient(srv, "tok")
	refs, err := c.LogRefs(context.Background(), 10, 5)
	if err != nil {
		t.Fatalf("LogRefs: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("got %d want 2", len(refs))
	}
	if refs[0].StepID != 300 || refs[0].StepName != "clone" {
		t.Fatalf("first ref %+v", refs[0])
	}
	if !strings.Contains(refs[0].URL, "/api/repos/10/logs/5/300") {
		t.Fatalf("url %q missing", refs[0].URL)
	}
	// error mapping: not found
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
	}))
	defer srv2.Close()
	c2 := newWoodpeckerTestClient(srv2, "tok")
	if _, err := c2.LogRefs(context.Background(), 10, 5); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound got %v", err)
	}
	// auth
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv3.Close()
	c3 := newWoodpeckerTestClient(srv3, "tok")
	if _, err := c3.LogRefs(context.Background(), 10, 5); !errors.Is(err, ErrValidation) {
		t.Fatalf("want ErrValidation got %v", err)
	}
}

func TestWoodpeckerRerun(t *testing.T) {
	tests := []struct {
		name    string
		code    int
		wantErr error
	}{
		{"success", http.StatusOK, nil},
		{"not found", http.StatusNotFound, ErrNotFound},
		{"auth", http.StatusUnauthorized, ErrValidation},
		{"conflict", http.StatusConflict, ErrConflict},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/repos/10/pipelines/5" {
					t.Errorf("path %s method %s", r.URL.Path, r.Method)
				}
				if r.Header.Get("Authorization") != "Bearer tok" {
					http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
					return
				}
				w.WriteHeader(tc.code)
				if tc.code != http.StatusOK {
					_, _ = w.Write([]byte(`{"message":"err"}`))
				} else {
					_, _ = w.Write([]byte(`{}`))
				}
			}))
			defer srv.Close()
			c := newWoodpeckerTestClient(srv, "tok")
			err := c.Rerun(context.Background(), 10, 5)
			if tc.wantErr != nil {
				if err == nil || !errors.Is(err, tc.wantErr) {
					t.Fatalf("err %v want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Rerun: %v", err)
			}
		})
	}
	// validation
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("should not hit server") }))
	defer srv.Close()
	c := newWoodpeckerTestClient(srv, "tok")
	if err := c.Rerun(context.Background(), 0, 1); !errors.Is(err, ErrValidation) {
		t.Fatalf("want validation got %v", err)
	}
	if err := c.Rerun(context.Background(), 10, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("want validation got %v", err)
	}
}

func TestWoodpeckerCancel(t *testing.T) {
	tests := []struct {
		name    string
		code    int
		wantErr error
	}{
		{"success", http.StatusNoContent, nil},
		{"not found", http.StatusNotFound, ErrNotFound},
		{"auth", http.StatusForbidden, ErrValidation},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/repos/10/pipelines/5/cancel" {
					t.Errorf("got %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.code)
				if tc.code != http.StatusNoContent {
					_, _ = w.Write([]byte(`{"message":"err"}`))
				}
			}))
			defer srv.Close()
			c := newWoodpeckerTestClient(srv, "tok")
			err := c.Cancel(context.Background(), 10, 5)
			if tc.wantErr != nil {
				if err == nil || !errors.Is(err, tc.wantErr) {
					t.Fatalf("err %v want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Cancel: %v", err)
			}
		})
	}
}

func TestWoodpeckerBaseURLNormalization(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/repos/10/pipelines/1" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "number": 1, "status": "success"})
	}))
	defer srv.Close()
	for _, base := range []string{srv.URL, srv.URL + "/", srv.URL + "/api", srv.URL + "/api/"} {
		c := NewWoodpeckerClient(WoodpeckerConfig{BaseURL: base, Token: "tok", HTTPClient: srv.Client()})
		_, err := c.GetRun(context.Background(), 10, 1)
		if err != nil {
			t.Fatalf("base %q: %v", base, err)
		}
	}
}

func TestWoodpeckerListRunsAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode([]any{})
	}))
	defer srv.Close()
	cBad := newWoodpeckerTestClient(srv, "bad")
	if _, err := cBad.ListRuns(context.Background(), 10, 10); !errors.Is(err, ErrValidation) {
		t.Fatalf("want ErrValidation got %v", err)
	}
	cGood := newWoodpeckerTestClient(srv, "good")
	if _, err := cGood.ListRuns(context.Background(), 10, 10); err != nil {
		t.Fatalf("good token should succeed: %v", err)
	}
}
