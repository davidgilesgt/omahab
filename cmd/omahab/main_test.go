package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apiclient"
)

func TestHintForError_401(t *testing.T) {
	err := &apiclient.APIError{StatusCode: 401, Code: "unauthorized", Message: "unauthorized"}
	hint := hintForError(err)
	if !strings.Contains(hint, "OMAHAB_TOKEN") || !strings.Contains(hint, "omahab login") {
		t.Fatalf("401 hint = %q, want contains OMAHAB_TOKEN and omahab login", hint)
	}
}

func TestHintForError_403(t *testing.T) {
	err := &apiclient.APIError{StatusCode: 403, Code: "forbidden", Message: "forbidden"}
	hint := hintForError(err)
	if !strings.Contains(hint, "OMAHAB_TOKEN") && !strings.Contains(hint, "login") {
		t.Fatalf("403 hint = %q, want auth hint", hint)
	}
}

func TestHintForError_404(t *testing.T) {
	err := &apiclient.APIError{StatusCode: 404, Code: "not_found", Message: "not found"}
	hint := hintForError(err)
	if !strings.Contains(hint, "list") {
		t.Fatalf("404 hint = %q, want contains list", hint)
	}
	// also check that it mentions resource list pattern
	if !strings.Contains(strings.ToLower(hint), "list") {
		t.Fatalf("404 hint missing list: %q", hint)
	}
}

func TestHintForError_Timeout(t *testing.T) {
	err := context.DeadlineExceeded
	hint := hintForError(err)
	if !strings.Contains(hint, "--server") || !strings.Contains(hint, "Tailscale") {
		t.Fatalf("timeout hint = %q, want --server and Tailscale", hint)
	}
	// also test wrapped timeout via string
	err2 := errors.New("Client.Timeout exceeded while awaiting headers")
	hint2 := hintForError(err2)
	if !strings.Contains(hint2, "--server") {
		t.Fatalf("timeout hint2 = %q, want --server", hint2)
	}
}

func TestHintForError_NoHint(t *testing.T) {
	err := errors.New("some random error")
	hint := hintForError(err)
	if hint != "" {
		t.Fatalf("unexpected hint for generic error: %q", hint)
	}
	err500 := &apiclient.APIError{StatusCode: 500, Code: "internal", Message: "boom"}
	hint500 := hintForError(err500)
	if hint500 != "" {
		t.Fatalf("unexpected hint for 500: %q", hint500)
	}
}

func TestHandleFailure_ReturnsErrorNotNil(t *testing.T) {
	// Ensure handleFailure never swallows errors - it must return non-nil for non-nil input
	flagJSON = false
	err := &apiclient.APIError{StatusCode: 401, Message: "unauthorized"}
	got := handleFailure(err)
	if got == nil {
		t.Fatal("handleFailure returned nil for 401, want non-nil to ensure exit 1")
	}
	// Also for generic error
	got2 := handleFailure(errors.New("boom"))
	if got2 == nil {
		t.Fatal("handleFailure returned nil for generic error")
	}
	// Nil input should return nil (no error)
	if got := handleFailure(nil); got != nil {
		t.Fatalf("handleFailure(nil) = %v, want nil", got)
	}
}

func TestHandleFailure_JSONReturnsError(t *testing.T) {
	flagJSON = true
	defer func() { flagJSON = false }()
	err := &apiclient.APIError{StatusCode: 404, Message: "not found"}
	got := handleFailure(err)
	if got == nil {
		t.Fatal("handleFailure in json mode returned nil, want non-nil")
	}
	if _, ok := got.(*printedError); !ok {
		t.Fatalf("handleFailure should return *printedError, got %T", got)
	}
}

func TestIsTimeoutError(t *testing.T) {
	if !isTimeoutError(context.DeadlineExceeded) {
		t.Fatal("expected timeout for DeadlineExceeded")
	}
	if isTimeoutError(errors.New("random")) {
		t.Fatal("unexpected timeout for random error")
	}
	if !isTimeoutError(errors.New("timeout while connecting")) {
		t.Fatal("expected timeout for string containing timeout")
	}
}

func TestHintForError_MessageContains401(t *testing.T) {
	err := errors.New("request failed with 401 Unauthorized")
	hint := hintForError(err)
	if !strings.Contains(hint, "OMAHAB_TOKEN") {
		t.Fatalf("hint for 401 string = %q, want OMAHAB_TOKEN", hint)
	}
}

func TestHandleFailure_HintMapping_Table(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantHint string
	}{
		{"401", &apiclient.APIError{StatusCode: http.StatusUnauthorized}, "OMAHAB_TOKEN"},
		{"404", &apiclient.APIError{StatusCode: http.StatusNotFound}, "list"},
		{"timeout", context.DeadlineExceeded, "--server"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hint := hintForError(tc.err)
			if !strings.Contains(hint, tc.wantHint) {
				t.Fatalf("hint = %q, want contains %q", hint, tc.wantHint)
			}
			// also ensure handleFailure returns error
			flagJSON = false
			got := handleFailure(tc.err)
			if got == nil {
				t.Fatalf("handleFailure returned nil for %s", tc.name)
			}
		})
	}
}
func TestRootRegistersAllTopLevel(t *testing.T) {
	root := newRootCmd()
	for _, name := range []string{"sync", "workspace", "backup", "backup-drive", "system", "doctor", "console", "setup"} {
		if c, _, err := root.Find([]string{name}); err != nil || c == nil || c.Name() != name {
			t.Fatalf("top-level %q not registered", name)
		}
	}
}
