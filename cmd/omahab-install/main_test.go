package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/installer"
)

func TestRunTailscaleUpStreamsOutputBeforeCommandExit(t *testing.T) {
	t.Parallel()

	const loginURL = "https://login.tailscale.com/a/test-token"
	var ui bytes.Buffer
	wroteURL := make(chan struct{})
	releaseCommand := make(chan struct{})
	commandFinished := make(chan struct{})
	probes := installer.Probes{
		CommandStream: func(ctx context.Context, combined io.Writer, name string, args ...string) error {
			if name != "tailscale" || len(args) != 1 || args[0] != "up" {
				return fmt.Errorf("unexpected command: %s %v", name, args)
			}
			if _, err := fmt.Fprintf(combined, "To authenticate, visit:\n\n\t%s\n", loginURL); err != nil {
				return err
			}
			close(wroteURL)
			select {
			case <-releaseCommand:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}

	go func() {
		runTailscaleUp(context.Background(), output{ui: &ui}, probes)
		close(commandFinished)
	}()

	select {
	case <-wroteURL:
	case <-time.After(time.Second):
		t.Fatal("tailscale login URL was not streamed")
	}
	select {
	case <-commandFinished:
		t.Fatal("tailscale command exited before streaming assertion")
	default:
	}
	if got := ui.String(); !strings.Contains(got, loginURL) {
		t.Fatalf("UI output before command exit = %q, want login URL", got)
	}

	close(releaseCommand)
	select {
	case <-commandFinished:
	case <-time.After(time.Second):
		t.Fatal("runTailscaleUp did not finish after command exit")
	}
	if got := strings.Count(ui.String(), loginURL); got != 1 {
		t.Fatalf("login URL rendered %d times, want once", got)
	}
}
