package knowledge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPKarakeepSearch_AuthPaginationError(t *testing.T) {
	// table for auth and error mapping
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// this server not used directly; each subtest creates its own
	}))
	srv.Close()
	tests := []struct {
		name      string
		token     string
		limit     int
		offset    int
		handler   func(w http.ResponseWriter, r *http.Request)
		wantIs    error
	}{
		{
			name:  "auth bearer sent",
			token: "bearer123",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "Bearer bearer123" {
					http.Error(w, "bad auth", 401)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"bookmarks": []any{}})
			},
		},
		{
			name: "no token no header",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "" {
					http.Error(w, "should empty", 401)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"bookmarks": []any{}})
			},
		},
		{
			name:  "pagination limit offset",
			token: "t", limit: 5, offset: 10,
			handler: func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if q.Get("limit") != "5" || q.Get("offset") != "10" {
					http.Error(w, "pagination mismatch limit="+q.Get("limit")+" offset="+q.Get("offset"), 400)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"bookmarks": []any{}})
			},
		},
		{
			name: "search param",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("search") != "golang" {
					http.Error(w, "search mismatch", 400)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"bookmarks": []any{}})
			},
		},
		{
			name: "401 forbidden",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "unauthorized", 401)
			},
			wantIs: ErrForbidden,
		},
		{
			name: "404 not found",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "not found", 404)
			},
			wantIs: ErrNotFound,
		},
		{
			name: "400 validation",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "bad", 400)
			},
			wantIs: ErrValidation,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(tc.handler))
			defer s.Close()
			c := NewKarakeepClientWithHTTP(s.URL, tc.token, s.Client())
			// For search param test we need to pass golang; for others use golang as well but handler will ignore or check? For auth tests handler checks only auth, not search, so passing golang is okay. For pagination test handler checks limit/offset, not search, so also pass golang. So all pass golang except where not checking search.
			query := "golang"
			if tc.name == "pagination limit offset" || tc.name == "auth bearer sent" || tc.name == "no token no header" {
				query = "golang"
			}
			if tc.name == "401 forbidden" || tc.name == "404 not found" || tc.name == "400 validation" {
				query = "golang"
			}
			_, err := c.Search(context.Background(), query, SearchOptions{Limit: tc.limit, Offset: tc.offset})
			if tc.wantIs != nil {
				if err == nil {
					t.Fatalf("want error %v got nil", tc.wantIs)
				}
				if !isErr(err, tc.wantIs) {
					t.Fatalf("want %v got %v", tc.wantIs, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
		})
	}
}

func TestHTTPKarakeepGetCaptureList(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/bookmarks/bm1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method", 405)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "bm1", "url": "https://example.com", "title": "Example", "tags": []string{"t1"}, "snippet": "snippet text"})
	})
	mux.HandleFunc("/api/v1/bookmarks", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// search
			_ = json.NewEncoder(w).Encode(map[string]any{"bookmarks": []map[string]any{{"id": "bm2", "url": "https://a.com", "title": "A", "tags": []any{}, "snippet": "snip"}}})
		case http.MethodPost:
			// capture
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["url"] == "" {
				http.Error(w, "missing url", 400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "new123"})
		default:
			http.Error(w, "method", 405)
		}
	})
	mux.HandleFunc("/api/v1/tags", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]string{"t1", "t2"})
	})
	s := httptest.NewServer(mux)
	defer s.Close()
	c := NewKarakeepClientWithHTTP(s.URL, "tok", s.Client())
	// Deep link check via Get
	bm, err := c.GetBookmark(context.Background(), "bm1")
	if err != nil {
		t.Fatalf("GetBookmark: %v", err)
	}
	if bm.ID != "bm1" || bm.URL != "https://example.com" {
		t.Fatalf("bookmark mismatch: %+v", bm)
	}
	if bm.DeepLink != s.URL+"/bookmarks/bm1" {
		t.Fatalf("deep link want %q got %q", s.URL+"/bookmarks/bm1", bm.DeepLink)
	}
	// Search deep link
	bms, err := c.Search(context.Background(), "", SearchOptions{})
	if err != nil || len(bms) != 1 {
		t.Fatalf("Search: %v %v", err, bms)
	}
	if bms[0].DeepLink != s.URL+"/bookmarks/bm2" {
		t.Fatalf("search deep link %q", bms[0].DeepLink)
	}
	// Capture
	id, err := c.CaptureBookmark(context.Background(), "https://example.com", "Example", CaptureOptions{Tags: []string{"t1"}})
	if err != nil || id != "new123" {
		t.Fatalf("CaptureBookmark: %v id=%q", err, id)
	}
	id2, err := c.CaptureArticle(context.Background(), "https://example.com/article", CaptureOptions{})
	if err != nil || id2 != "new123" {
		t.Fatalf("CaptureArticle: %v id=%q", err, id2)
	}
	// validation empty url
	if _, err := c.CaptureBookmark(context.Background(), "", "t", CaptureOptions{}); err == nil {
		t.Fatalf("empty url should fail")
	}
	tags, err := c.ListTags(context.Background())
	if err != nil || len(tags) != 2 {
		t.Fatalf("ListTags: %v %v", err, tags)
	}
	// 404 mapping
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", 404)
	}))
	defer s2.Close()
	c2 := NewKarakeepClientWithHTTP(s2.URL, "", s2.Client())
	if _, err := c2.GetBookmark(context.Background(), "missing"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want not found, got %v", err)
	}
}
