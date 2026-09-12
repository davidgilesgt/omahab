package api

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/omahab/omahab/internal/store"
)

// Store-wrapped backend errors must map to stable codes at the boundary,
// even when a controlplane method skips translateError (e.g. wizard
// recovery confirm, Cloudflare token verify): 404/409/400, never 500.
func TestStoreSentinelMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("%w: nope", store.ErrNotFound), http.StatusNotFound, CodeNotFound},
		{fmt.Errorf("%w: clash", store.ErrConflict), http.StatusConflict, CodeConflict},
		{fmt.Errorf("%w: bad", store.ErrValidation), http.StatusBadRequest, CodeBadRequest},
	}
	for _, c := range cases {
		if got := httpStatus(c.err); got != c.status {
			t.Errorf("httpStatus(%v) = %d, want %d", c.err, got, c.status)
		}
		if got := errorCode(c.err); got != c.code {
			t.Errorf("errorCode(%v) = %q, want %q", c.err, got, c.code)
		}
		if got := errorMessage(c.err); got == "internal error" {
			t.Errorf("errorMessage(%v) leaked internal placeholder", c.err)
		}
	}
}
