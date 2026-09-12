package projects

import (
	"errors"
	"strings"
	"testing"
)

func TestDeriveSlug(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"lowercases", "My Blog", "my-blog"},
		{"collapses punctuation runs", "Hello,  World!", "hello-world"},
		{"trims dashes", "--Acme--", "acme"},
		{"already valid", "blog-2024", "blog-2024"},
	}
	for _, tc := range cases {
		got, err := DeriveSlug(tc.in)
		if err != nil {
			t.Fatalf("%s: DeriveSlug(%q): %v", tc.name, tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("%s: DeriveSlug(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}

	long, err := DeriveSlug(strings.Repeat("A", 100))
	if err != nil {
		t.Fatalf("long name: %v", err)
	}
	if len(long) != 63 || strings.HasSuffix(long, "-") {
		t.Fatalf("long name derived %q, want 63 chars without trailing '-'", long)
	}

	for _, bad := range []string{"", "   ", "!!!", "---"} {
		if _, err := DeriveSlug(bad); !errors.Is(err, ErrValidation) {
			t.Fatalf("DeriveSlug(%q) = %v, want ErrValidation", bad, err)
		}
	}
}
