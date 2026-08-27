package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/installer"
)

type synchronizedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

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

func withStdin(t *testing.T, input string) func() {
	t.Helper()
	orig := os.Stdin
	origIsTerminal := isTerminal
	isTerminal = func(f *os.File) bool { return true }
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	w.Close()
	os.Stdin = r
	stdinMu.Lock()
	stdinReader = nil
	stdinReaderFile = nil
	stdinMu.Unlock()
	return func() {
		os.Stdin = orig
		r.Close()
		isTerminal = origIsTerminal
		stdinMu.Lock()
		stdinReader = nil
		stdinReaderFile = nil
		stdinMu.Unlock()
	}
}

func testOutput() (output, *bytes.Buffer) {
	var buf bytes.Buffer
	out := output{
		ui:             &buf,
		data:           io.Discard,
		isTTY:          true,
		color:          false,
		caps:           installer.Capabilities{IsTTY: true, ColorEnabled: false, Width: 80},
		nonInteractive: false,
		jsonMode:       false,
	}
	return out, &buf
}

func TestGuideCloudflare_BlankRepromptsNotSkipped(t *testing.T) {
	cleanup := withStdin(t, "\nexample.com\nskip\n\n")
	defer cleanup()
	out, buf := testOutput()
	// probes not needed for Cloudflare domain/token, but provide empty
	probes := installer.Probes{}
	if err := guideCloudflare(context.Background(), out, probes); err != nil {
		t.Fatalf("guideCloudflare failed: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "Skipped Cloudflare setup") {
		t.Fatalf("blank apex input incorrectly skipped Cloudflare; output:\n%s", got)
	}
	if !strings.Contains(got, "Domain is required") {
		t.Fatalf("expected reprompt for blank domain, missing 'Domain is required'; output:\n%s", got)
	}
	if !strings.Contains(got, "✓ domain example.com") {
		t.Fatalf("expected domain success after reprompt; output:\n%s", got)
	}
	if !strings.Contains(got, "Cloudflare") {
		t.Fatalf("expected Cloudflare header; output:\n%s", got)
	}
}

func TestGuideCloudflare_ExplicitSkipStillWorks(t *testing.T) {
	for _, input := range []string{"skip\n", "s\n", "SKIP\n"} {
		t.Run(input, func(t *testing.T) {
			cleanup := withStdin(t, input)
			defer cleanup()
			out, buf := testOutput()
			if err := guideCloudflare(context.Background(), out, installer.Probes{}); err != nil {
				t.Fatalf("guideCloudflare failed: %v", err)
			}
			got := buf.String()
			if !strings.Contains(got, "Skipped") {
				t.Fatalf("explicit skip %q did not produce Skipped; output:\n%s", input, got)
			}
			if strings.Contains(got, "Domain is required") {
				t.Fatalf("explicit skip %q incorrectly triggered reprompt; output:\n%s", input, got)
			}
		})
	}
}

func TestGuideTailscale_CheckingAndTransition(t *testing.T) {
	cleanup := withStdin(t, "skip\n")
	defer cleanup()
	var buf bytes.Buffer
	out := output{
		ui:             &buf,
		data:           io.Discard,
		isTTY:          true,
		color:          false,
		caps:           installer.Capabilities{IsTTY: true, ColorEnabled: false, Width: 80},
		nonInteractive: false,
		jsonMode:       false,
	}
	// Simulate Already Running — should show checking and transition if we arrange to go via success path without skip?
	// For this test we simulate NeedsLogin then Running after one skip? Instead test success path directly.
	// Use status that immediately reports Running so guideTailscale succeeds without prompting.
	probes := installer.Probes{
		CommandExists: func(name string) bool { return name == "tailscale" },
		CommandOutput: func(ctx context.Context, name string, args ...string) (string, error) {
			if name == "tailscale" && len(args) == 1 && args[0] == "version" {
				return "tailscale version 1.80.0", nil
			}
			if name == "tailscale" && len(args) >= 2 && args[0] == "status" {
				return `{"BackendState":"Running"}`, nil
			}
			if name == "tailscale" && len(args) >= 2 && args[0] == "ip" {
				return "100.64.0.1\n", nil
			}
			return "", nil
		},
	}
	if err := guideTailscale(context.Background(), out, probes); err != nil {
		t.Fatalf("guideTailscale failed: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "Checking Tailscale status") {
		t.Fatalf("expected checking state/spinner; output:\n%s", got)
	}
	if !strings.Contains(got, "Tailscale is up") {
		t.Fatalf("expected Tailscale success; output:\n%s", got)
	}
	if !strings.Contains(got, "continuing to Cloudflare setup") {
		t.Fatalf("expected explicit Cloudflare transition after Tailscale success; output:\n%s", got)
	}
}

func TestGuidedFlow_ExtraEnterDoesNotSkipCloudflare(t *testing.T) {
	// stdin: 1st Enter for Tailscale re-check, 2nd extra Enter (should reprompt Cloudflare), then valid domain, skip tokens
	input := "\n\nexample.com\nskip\n\n"
	cleanup := withStdin(t, input)
	defer cleanup()
	out, buf := testOutput()
	// Probes that simulate NeedsLogin -> NeedsLogin -> Running
	statusCalls := 0
	probes := installer.Probes{
		CommandExists: func(name string) bool { return name == "tailscale" },
		CommandOutput: func(ctx context.Context, name string, args ...string) (string, error) {
			if name == "tailscale" && len(args) == 1 && args[0] == "version" {
				return "tailscale version 1.80.0", nil
			}
			if name == "tailscale" && len(args) >= 2 && args[0] == "status" && args[1] == "--json" {
				statusCalls++
				if statusCalls <= 2 {
					return `{"BackendState":"NeedsLogin"}`, nil
				}
				return `{"BackendState":"Running"}`, nil
			}
			if name == "tailscale" && len(args) == 2 && args[0] == "ip" && args[1] == "-4" {
				if statusCalls >= 3 {
					return "100.64.0.42\n", nil
				}
				return "", fmt.Errorf("no ip")
			}
			if name == "tailscale" && len(args) == 1 && args[0] == "status" {
				return `{"BackendState":"NeedsLogin"}`, nil
			}
			return "", fmt.Errorf("unexpected %s %v", name, args)
		},
		CommandStream: func(ctx context.Context, combined io.Writer, name string, args ...string) error {
			if name == "tailscale" && len(args) == 1 && args[0] == "up" {
				fmt.Fprintln(combined, "To authenticate, visit:\n\n\thttps://login.tailscale.com/a/fake-fixed")
				return nil
			}
			return nil
		},
	}
	ctx := context.Background()
	if err := guideTailscale(ctx, out, probes); err != nil {
		t.Fatalf("guideTailscale failed: %v, output:\n%s", err, buf.String())
	}
	gotAfterTailscale := buf.String()
	if !strings.Contains(gotAfterTailscale, "Checking Tailscale status") {
		t.Fatalf("expected checking progression; output:\n%s", gotAfterTailscale)
	}
	if !strings.Contains(gotAfterTailscale, "continuing to Cloudflare setup") {
		t.Fatalf("expected Cloudflare transition; output:\n%s", gotAfterTailscale)
	}
	if c := strings.Count(gotAfterTailscale, "https://login.tailscale.com/a/fake-fixed"); c != 2 {
		// two tailscale up runs each should emit URL once; duplicate within single run would be >2
		// Allow 2 (one per up). If our code duplicated within a single run, count would be higher per run.
		// We check per-run earlier, here just ensure not grossly duplicated.
		t.Fatalf("login URL count %d want 2 (one per tailscale up); output:\n%s", c, gotAfterTailscale)
	}
	// Now run Cloudflare with the same shared stdin (extra Enter already buffered)
	if err := guideCloudflare(ctx, out, installer.Probes{}); err != nil {
		t.Fatalf("guideCloudflare failed: %v, output:\n%s", err, buf.String())
	}
	got := buf.String()
	if strings.Contains(got, "Skipped Cloudflare setup") {
		t.Fatalf("extra Enter incorrectly skipped Cloudflare; output:\n%s", got)
	}
	if !strings.Contains(got, "Domain is required") {
		t.Fatalf("extra blank Enter should have caused reprompt, missing 'Domain is required'; output:\n%s", got)
	}
	if !strings.Contains(got, "✓ domain example.com") {
		t.Fatalf("expected domain success after reprompt; output:\n%s", got)
	}
	if !strings.Contains(got, "Cloudflare") {
		t.Fatalf("expected Cloudflare header/transition; output:\n%s", got)
	}
}

func TestGuidedFlow_ExplicitSkipAfterTailscale(t *testing.T) {
	// Verify explicit skip still works after successful Tailscale
	input := "skip\nskip\n"
	cleanup := withStdin(t, input)
	defer cleanup()
	out, buf := testOutput()
	probes := installer.Probes{
		CommandExists: func(name string) bool { return name == "tailscale" },
		CommandOutput: func(ctx context.Context, name string, args ...string) (string, error) {
			if name == "tailscale" && len(args) == 1 && args[0] == "version" {
				return "tailscale version 1.80.0", nil
			}
			if name == "tailscale" && len(args) >= 2 && args[0] == "status" {
				return `{"BackendState":"Running"}`, nil
			}
			if name == "tailscale" && len(args) >= 2 && args[0] == "ip" {
				return "100.64.0.5\n", nil
			}
			return "", nil
		},
	}
	if err := guideTailscale(context.Background(), out, probes); err != nil {
		t.Fatalf("guideTailscale: %v", err)
	}
	if err := guideCloudflare(context.Background(), out, installer.Probes{}); err != nil {
		t.Fatalf("guideCloudflare: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "Skipped") {
		t.Fatalf("explicit skip after Tailscale should still skip Cloudflare; output:\n%s", got)
	}
}

func TestTailscaleStatusCheckWithFeedback_SpinnerAnimates(t *testing.T) {
	origIsTerminal := isTerminal
	isTerminal = func(f *os.File) bool { return true }
	defer func() { isTerminal = origIsTerminal }()

	blocked := make(chan struct{})
	probes := installer.Probes{
		CommandExists: func(name string) bool { return name == "tailscale" },
		CommandOutput: func(ctx context.Context, name string, args ...string) (string, error) {
			if name == "tailscale" && len(args) == 1 && args[0] == "version" {
				return "tailscale version 1.80.0", nil
			}
			if name == "tailscale" && len(args) >= 2 && args[0] == "status" && args[1] == "--json" {
				<-blocked
				return `{"BackendState":"Running"}`, nil
			}
			if name == "tailscale" && len(args) == 2 && args[0] == "ip" && args[1] == "-4" {
				return "100.64.0.9\n", nil
			}
			if name == "tailscale" && len(args) == 1 && args[0] == "status" {
				<-blocked
				return `{"BackendState":"Running"}`, nil
			}
			return "", nil
		},
	}
	var buf synchronizedBuffer
	out := output{
		ui:             &buf,
		data:           io.Discard,
		isTTY:          true,
		color:          false,
		caps:           installer.Capabilities{IsTTY: true, ColorEnabled: true, Width: 80},
		nonInteractive: false,
		jsonMode:       false,
	}
	done := make(chan struct{})
	var loggedIn bool
	var ip string
	go func() {
		_, loggedIn, ip, _ = tailscaleStatusCheckWithFeedback(context.Background(), out, probes)
		close(done)
	}()
	// Observe at least two spinner frames before releasing the probe.
	deadline := time.Now().Add(2 * time.Second)
	sawTwo := false
	for time.Now().Before(deadline) {
		s := buf.String()
		if strings.Count(s, "Checking Tailscale status") >= 2 {
			sawTwo = true
			break
		}
		select {
		case <-done:
			break
		default:
		}
		if sawTwo {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !sawTwo {
		t.Fatalf("expected at least two spinner frames before release, got %q (count %d)", buf.String(), strings.Count(buf.String(), "Checking Tailscale status"))
	}
	close(blocked)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tailscaleStatusCheckWithFeedback did not finish after release")
	}
	if !loggedIn || ip == "" {
		t.Fatalf("expected loggedIn with IP after release, got loggedIn=%v ip=%q", loggedIn, ip)
	}
	s := buf.String()
	if c := strings.Count(s, "\r\033[K"); c != 1 {
		t.Fatalf("expected exactly one clear sequence \\r\\033[K, got %d in %q", c, s)
	}
	// Ensure plain fallback not used for capable TTY (should be spinner, not newline single line).
	// The spinner path uses \\r without newline; plain would be \"Checking...\\n\".
	// We already verified spinner frames, but also ensure no extra plain line remains.
}

func TestTailscaleStatusCheckWithFeedback_PlainFallback(t *testing.T) {
	origIsTerminal := isTerminal
	isTerminal = func(f *os.File) bool { return false }
	defer func() { isTerminal = origIsTerminal }()

	probes := installer.Probes{
		CommandExists: func(name string) bool { return true },
		CommandOutput: func(ctx context.Context, name string, args ...string) (string, error) {
			if name == "tailscale" && len(args) == 1 && args[0] == "version" {
				return "tailscale version 1.80.0", nil
			}
			if name == "tailscale" && len(args) >= 2 && args[0] == "status" {
				return `{"BackendState":"Running"}`, nil
			}
			if name == "tailscale" && len(args) == 2 && args[0] == "ip" {
				return "100.64.0.2\n", nil
			}
			return "", nil
		},
	}
	var buf bytes.Buffer
	out := output{
		ui:             &buf,
		data:           io.Discard,
		isTTY:          true,
		color:          false,
		caps:           installer.Capabilities{IsTTY: true, ColorEnabled: false, Width: 80},
		nonInteractive: false,
		jsonMode:       false,
	}
	_, loggedIn, ip, _ := tailscaleStatusCheckWithFeedback(context.Background(), out, probes)
	if !loggedIn || ip == "" {
		t.Fatalf("expected success")
	}
	s := buf.String()
	if !strings.Contains(s, "Checking Tailscale status") {
		t.Fatalf("expected plain checking line for non-capable TTY, got %q", s)
	}
	if strings.Contains(s, "\r\033[K") {
		t.Fatalf("plain fallback should not emit spinner clear sequence, got %q", s)
	}
}
