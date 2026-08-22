package scm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newForgejoTestClient(srv *httptest.Server, token string, secrets SecretStore) ForgejoClient {
	return NewForgejoClient(ForgejoConfig{
		BaseURL:    srv.URL,
		Token:      token,
		HTTPClient: srv.Client(),
		SecretStore: secrets,
	})
}

func TestForgejoCreateRepo(t *testing.T) {
	tests := []struct {
		name      string
		token     string
		in        CreateRepoInput
		handler   func(w http.ResponseWriter, r *http.Request)
		wantOwner string
		wantErr   error
	}{
		{
			name:  "success user repo",
			token: "tok",
			in:    CreateRepoInput{Owner: "", Name: "myrepo", Description: "desc", Private: true, DefaultBranch: "main"},
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/user/repos" {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				if r.Header.Get("Authorization") != "token tok" {
					http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
					return
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["name"] != "myrepo" || body["private"] != true {
					http.Error(w, `{"message":"bad body"}`, http.StatusBadRequest)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": 42, "name": "myrepo", "full_name": "alice/myrepo",
					"private": true, "clone_url": "https://git.example.com/alice/myrepo.git",
					"default_branch": "main", "has_actions": true,
					"owner": map[string]any{"login": "alice"},
				})
			},
			wantOwner: "alice",
		},
		{
			name:  "success org repo",
			token: "tok",
			in:    CreateRepoInput{Owner: "myorg", Name: "myrepo", Private: true},
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/orgs/myorg/repos" {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": 43, "name": "myrepo", "full_name": "myorg/myrepo",
					"private": true, "clone_url": "https://git.example.com/myorg/myrepo.git",
					"default_branch": "master", "has_actions": true,
					"owner": map[string]any{"login": "myorg"},
				})
			},
			wantOwner: "myorg",
		},
		{
			name:  "fallback to user when org not found",
			token: "tok",
			in:    CreateRepoInput{Owner: "alice", Name: "myrepo", Private: true},
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/orgs/alice/repos" {
					http.Error(w, `{"message":"org not found"}`, http.StatusNotFound)
					return
				}
				if r.URL.Path == "/api/v1/user/repos" {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": 1, "name": "myrepo", "full_name": "alice/myrepo",
						"private": true, "clone_url": "https://git.example.com/alice/myrepo.git",
						"default_branch": "master", "has_actions": true,
						"owner": map[string]any{"login": "alice"},
					})
					return
				}
				http.Error(w, "not found", http.StatusNotFound)
			},
			wantOwner: "alice",
		},
		{
			name:  "auth missing",
			token: "bad",
			in:    CreateRepoInput{Name: "x", Private: true},
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "token tok" {
					http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
					return
				}
				w.WriteHeader(http.StatusOK)
			},
			wantErr: ErrValidation,
		},
		{
			name:  "conflict",
			token: "tok",
			in:    CreateRepoInput{Name: "dup", Private: true},
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"message":"already exists"}`, http.StatusConflict)
			},
			wantErr: ErrConflict,
		},
		{
			name:    "validation missing name",
			token:   "tok",
			in:      CreateRepoInput{Name: "", Private: true},
			handler: func(w http.ResponseWriter, r *http.Request) { t.Fatalf("should not hit server") },
			wantErr: ErrValidation,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(tc.handler))
			defer srv.Close()
			client := newForgejoTestClient(srv, tc.token, nil)
			// For auth test, server expects "tok" regardless of client token.
			got, err := client.CreateRepo(context.Background(), tc.in)
			if tc.wantErr != nil {
				if err == nil || !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateRepo: %v", err)
			}
			if got.Owner != tc.wantOwner {
				t.Fatalf("owner %q != %q", got.Owner, tc.wantOwner)
			}
			if !got.Private {
				t.Fatal("expected private")
			}
		})
	}
}

func TestForgejoGetRepo(t *testing.T) {
	tests := []struct {
		name    string
		ref     RepoRef
		handler func(w http.ResponseWriter, r *http.Request)
		wantErr error
	}{
		{
			name: "success",
			ref:  RepoRef{Owner: "alice", Name: "myrepo"},
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/repos/alice/myrepo" {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": 5, "name": "myrepo", "full_name": "alice/myrepo",
					"private": true, "clone_url": "https://git.example.com/alice/myrepo.git",
					"default_branch": "main", "has_actions": false,
					"owner": map[string]any{"login": "alice"},
				})
			},
		},
		{
			name: "not found",
			ref:  RepoRef{Owner: "alice", Name: "missing"},
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			},
			wantErr: ErrNotFound,
		},
		{
			name: "auth",
			ref:  RepoRef{Owner: "alice", Name: "myrepo"},
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "token good" {
					http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"id":1,"name":"myrepo","owner":{"login":"alice"}}`))
			},
			wantErr: ErrValidation,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(tc.handler))
			defer srv.Close()
			token := "good"
			if tc.name == "auth" {
				token = "bad"
			}
			client := newForgejoTestClient(srv, token, nil)
			got, err := client.GetRepo(context.Background(), tc.ref)
			if tc.wantErr != nil {
				if err == nil || !errors.Is(err, tc.wantErr) {
					t.Fatalf("err %v want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetRepo: %v", err)
			}
			if got.Name != tc.ref.Name {
				t.Fatalf("name %q != %q", got.Name, tc.ref.Name)
			}
		})
	}
}

func TestForgejoDeleteRepo(t *testing.T) {
	tests := []struct {
		name    string
		ref     RepoRef
		code    int
		wantErr error
	}{
		{"success", RepoRef{"alice", "myrepo"}, http.StatusNoContent, nil},
		{"not found", RepoRef{"alice", "missing"}, http.StatusNotFound, ErrNotFound},
		{"auth", RepoRef{"alice", "myrepo"}, http.StatusForbidden, ErrValidation},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete {
					t.Errorf("method %s", r.Method)
				}
				w.WriteHeader(tc.code)
				if tc.code != http.StatusNoContent {
					_, _ = w.Write([]byte(`{"message":"err"}`))
				}
			}))
			defer srv.Close()
			c := newForgejoTestClient(srv, "tok", nil)
			err := c.DeleteRepo(context.Background(), tc.ref)
			if tc.wantErr != nil {
				if err == nil || !errors.Is(err, tc.wantErr) {
					t.Fatalf("err %v want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("DeleteRepo: %v", err)
			}
		})
	}
}

func TestForgejoSetActionsEnabled(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		handler func(w http.ResponseWriter, r *http.Request)
		wantErr error
	}{
		{
			name:    "disable",
			enabled: false,
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPatch {
					http.Error(w, "bad method", http.StatusMethodNotAllowed)
					return
				}
				var b map[string]any
				_ = json.NewDecoder(r.Body).Decode(&b)
				if b["has_actions"] != false {
					http.Error(w, `{"message":"expected false"}`, http.StatusBadRequest)
					return
				}
				w.WriteHeader(http.StatusOK)
			},
		},
		{
			name:    "enable",
			enabled: true,
			handler: func(w http.ResponseWriter, r *http.Request) {
				var b map[string]any
				_ = json.NewDecoder(r.Body).Decode(&b)
				if b["has_actions"] != true {
					http.Error(w, `{"message":"expected true"}`, http.StatusBadRequest)
					return
				}
				w.WriteHeader(http.StatusOK)
			},
		},
		{
			name:    "not found",
			enabled: false,
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			},
			wantErr: ErrNotFound,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(tc.handler))
			defer srv.Close()
			c := newForgejoTestClient(srv, "tok", nil)
			err := c.SetActionsEnabled(context.Background(), RepoRef{"alice", "myrepo"}, tc.enabled)
			if tc.wantErr != nil {
				if err == nil || !errors.Is(err, tc.wantErr) {
					t.Fatalf("err %v want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("SetActionsEnabled: %v", err)
			}
		})
	}
}

func TestForgejoPutPushMirror(t *testing.T) {
	tests := []struct {
		name    string
		secrets SecretStore
		in      MirrorInput
		handler func(w http.ResponseWriter, r *http.Request)
		wantErr error
	}{
		{
			name: "success with raw credential",
			in: MirrorInput{
				RemoteURL: "https://github.com/alice/mirror.git", RemoteName: "github",
				CredentialSecretRef: "secrettoken123", IntervalSeconds: 0, LFSEnabled: false,
			},
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/repos/alice/myrepo/push_mirrors" {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				var b map[string]any
				_ = json.NewDecoder(r.Body).Decode(&b)
				if b["remote_address"] != "https://github.com/alice/mirror.git" {
					http.Error(w, `{"message":"bad address"}`, http.StatusBadRequest)
					return
				}
				if b["remote_password"] != "secrettoken123" {
					http.Error(w, `{"message":"bad password"}`, http.StatusBadRequest)
					return
				}
				if b["sync_on_commit"] != true {
					http.Error(w, `{"message":"sync"}`, http.StatusBadRequest)
					return
				}
				// interval should be omitted or empty for 0
				if v, ok := b["interval"]; ok && v != "" {
					http.Error(w, `{"message":"interval should be empty"}`, http.StatusBadRequest)
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			},
		},
		{
			name: "success with secret store resolution",
			secrets: func() SecretStore {
				s := NewMemorySecretStore()
				_ = s.Put(context.Background(), "scm:project:ID", "github-mirror-token", "resolved-token")
				return s
			}(),
			in: MirrorInput{
				RemoteURL: "https://github.com/alice/mirror.git", RemoteName: "github",
				CredentialSecretRef: "scm:project:ID/github-mirror-token", IntervalSeconds: 3600, LFSEnabled: true,
			},
			handler: func(w http.ResponseWriter, r *http.Request) {
				var b map[string]any
				_ = json.NewDecoder(r.Body).Decode(&b)
				if b["remote_password"] != "resolved-token" {
					http.Error(w, `{"message":"bad resolved"}`, http.StatusBadRequest)
					return
				}
				if b["interval"] != "3600s" {
					http.Error(w, `{"message":"bad interval"}`, http.StatusBadRequest)
					return
				}
				w.WriteHeader(http.StatusOK)
			},
		},
		{
			name: "interval formatting and LFS explicit handling",
			in: MirrorInput{
				RemoteURL: "https://github.com/alice/mirror.git", RemoteName: "github",
				CredentialSecretRef: "tok", IntervalSeconds: 7200, LFSEnabled: true,
			},
			handler: func(w http.ResponseWriter, r *http.Request) {
				var b map[string]any
				_ = json.NewDecoder(r.Body).Decode(&b)
				if b["interval"] != "7200s" {
					http.Error(w, `{"message":"interval"}`, http.StatusBadRequest)
					return
				}
				w.WriteHeader(http.StatusOK)
			},
		},
		{
			name: "conflict then retry",
			in: MirrorInput{
				RemoteURL: "https://github.com/alice/mirror.git", RemoteName: "github",
				CredentialSecretRef: "tok",
			},
			handler: func() func(w http.ResponseWriter, r *http.Request) {
				calls := 0
				return func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "push_mirrors") {
						calls++
						if calls == 1 {
							http.Error(w, `{"message":"exists"}`, http.StatusConflict)
							return
						}
						w.WriteHeader(http.StatusOK)
						return
					}
					if r.Method == http.MethodDelete {
						w.WriteHeader(http.StatusOK)
						return
					}
					http.Error(w, "not found", http.StatusNotFound)
				}
			}(),
		},
		{
			name:    "validation remote url required",
			in:      MirrorInput{RemoteName: "github"},
			handler: func(w http.ResponseWriter, r *http.Request) { t.Fatal("should not hit server") },
			wantErr: ErrValidation,
		},
		{
			name: "not found maps to ErrNotFound",
			in: MirrorInput{
				RemoteURL: "https://github.com/alice/mirror.git", RemoteName: "github",
				CredentialSecretRef: "tok",
			},
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
			c := newForgejoTestClient(srv, "tok", tc.secrets)
			err := c.PutPushMirror(context.Background(), RepoRef{"alice", "myrepo"}, tc.in)
			if tc.wantErr != nil {
				if err == nil || !errors.Is(err, tc.wantErr) {
					t.Fatalf("err %v want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("PutPushMirror: %v", err)
			}
		})
	}
}

func TestForgejoDeletePushMirror(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/repos/alice/myrepo/push_mirrors/github" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "token tok" {
			http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := newForgejoTestClient(srv, "tok", nil)
	if err := c.DeletePushMirror(context.Background(), RepoRef{"alice", "myrepo"}, "github"); err != nil {
		t.Fatalf("DeletePushMirror: %v", err)
	}
	// error mapping
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
	}))
	defer srv2.Close()
	c2 := newForgejoTestClient(srv2, "tok", nil)
	if err := c2.DeletePushMirror(context.Background(), RepoRef{"alice", "myrepo"}, "github"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound got %v", err)
	}
	// auth
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv3.Close()
	c3 := newForgejoTestClient(srv3, "bad", nil)
	if err := c3.DeletePushMirror(context.Background(), RepoRef{"alice", "myrepo"}, "github"); !errors.Is(err, ErrValidation) {
		t.Fatalf("want ErrValidation got %v", err)
	}
}

func TestForgejoPutFileAndSeed(t *testing.T) {
	content := "when:\n  - event: push\n"
	tests := []struct {
		name    string
		file    string
		message string
	}{
		{"put file", "some/path.txt", "Add some/path.txt"},
		{"seed woodpecker", ".woodpecker.yaml", "Add .woodpecker.yaml (managed by Omahab)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					http.Error(w, "bad method", http.StatusMethodNotAllowed)
					return
				}
				expectedPath := "/api/v1/repos/alice/myrepo/contents/" + tc.file
				if r.URL.Path != expectedPath {
					t.Errorf("path %q != %q", r.URL.Path, expectedPath)
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				b64, _ := body["content"].(string)
				decoded, _ := base64.StdEncoding.DecodeString(b64)
				if string(decoded) != content {
					http.Error(w, `{"message":"bad content"}`, http.StatusBadRequest)
					return
				}
				if body["message"] != tc.message {
					http.Error(w, `{"message":"bad message"}`, http.StatusBadRequest)
					return
				}
				w.WriteHeader(http.StatusCreated)
			}))
			defer srv.Close()
			c := newForgejoTestClient(srv, "tok", nil).(*forgejoClient)
			var err error
			if tc.file == ".woodpecker.yaml" {
				err = c.SeedWoodpeckerConfig(context.Background(), RepoRef{"alice", "myrepo"}, content)
			} else {
				err = c.PutFile(context.Background(), RepoRef{"alice", "myrepo"}, tc.file, []byte(content), tc.message)
			}
			if err != nil {
				t.Fatalf("PutFile: %v", err)
			}
		})
	}
}

func TestForgejoAuthErrorMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := newForgejoTestClient(srv, "wrong", nil)
	_, err := c.GetRepo(context.Background(), RepoRef{"a", "b"})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("want ErrValidation got %v", err)
	}
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `internal`, http.StatusInternalServerError)
	}))
	defer srv2.Close()
	c2 := newForgejoTestClient(srv2, "tok", nil)
	_, err = c2.GetRepo(context.Background(), RepoRef{"a", "b"})
	if err == nil || strings.Contains(err.Error(), ErrValidation.Error()) {
		t.Fatalf("expected server error not validation, got %v", err)
	}
}

func TestForgejoValidation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("should not call server") }))
	defer srv.Close()
	c := newForgejoTestClient(srv, "tok", nil)
	if _, err := c.GetRepo(context.Background(), RepoRef{"", "x"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("want validation got %v", err)
	}
	if err := c.DeleteRepo(context.Background(), RepoRef{"", ""}); !errors.Is(err, ErrValidation) {
		t.Fatalf("want validation got %v", err)
	}
	if err := c.SetActionsEnabled(context.Background(), RepoRef{"a", ""}, false); !errors.Is(err, ErrValidation) {
		t.Fatalf("want validation got %v", err)
	}
}

func TestForgejoBaseURLNormalization(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/alice/myrepo" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 1, "name": "myrepo", "owner": map[string]any{"login": "alice"},
		})
	}))
	defer srv.Close()
	for _, base := range []string{srv.URL, srv.URL + "/", srv.URL + "/api/v1", srv.URL + "/api/v1/"} {
		c := NewForgejoClient(ForgejoConfig{BaseURL: base, Token: "tok", HTTPClient: srv.Client()})
		_, err := c.GetRepo(context.Background(), RepoRef{"alice", "myrepo"})
		if err != nil {
			t.Fatalf("base %q: %v", base, err)
		}
	}
}
