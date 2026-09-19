package controlplane

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEnsureOmahabBotRecoversRejectedTokenNameCollision(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/user" {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	b, _ := newSetupBackend(t, nil)
	b.forgejoBaseURLOverride = srv.URL
	if err := upsertSecret(context.Background(), b.secrets, "platform-app", "forgejo_token", "rejected-token"); err != nil {
		t.Fatal(err)
	}

	var generatedNames []string
	b.forgejoExec = func(_ context.Context, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case joined == "user list":
			return "1 omahab-bot omahab-bot@example.test", nil
		case strings.HasPrefix(joined, "user generate-access-token "):
			for i, arg := range args {
				if arg == "--token-name" && i+1 < len(args) {
					generatedNames = append(generatedNames, args[i+1])
					if args[i+1] == "omahab" {
						return "", errors.New("access token name has been used already")
					}
					return "replacement-token", nil
				}
			}
		}
		return "", errors.New("unexpected forgejo command: " + joined)
	}

	if err := b.ensureOmahabBot(context.Background(), "example.test"); err != nil {
		t.Fatalf("recover token: %v", err)
	}
	if len(generatedNames) != 2 || generatedNames[0] != "omahab" || !strings.HasPrefix(generatedNames[1], "omahab-recovery-") {
		t.Fatalf("token names = %v, want canonical then unique recovery name", generatedNames)
	}
	got, err := b.secrets.RevealByName(context.Background(), "platform-app", "forgejo_token")
	if err != nil {
		t.Fatal(err)
	}
	if got != "replacement-token" {
		t.Fatalf("stored token = %q, want replacement-token", got)
	}
}
