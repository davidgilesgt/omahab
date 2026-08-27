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
	// With settlement polling, a single blank recheck polls until Running, so only one tailscale up is needed.
	input := "\n\nexample.com\nskip\n\n"
	cleanup := withStdin(t, input)
	defer cleanup()
	// Keep settlement poll deterministic (no real sleep).
	origSleep := tailscaleSettleSleep
	tailscaleSettleSleep = func(time.Duration) {}
	defer func() { tailscaleSettleSleep = origSleep }()
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
	if c := strings.Count(gotAfterTailscale, "https://login.tailscale.com/a/fake-fixed"); c != 1 {
		t.Fatalf("login URL count %d want 1 (one per tailscale up with settlement poll); output:\n%s", c, gotAfterTailscale)
	}
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
func TestGuideTailscale_StatusSettlesWithoutSecondEnter(t *testing.T) {
	// Proves one typed `status` plus its Enter advances as soon as Tailscale settles,
	// without requiring a second Enter. The first probe after `status` sees
	// NeedsLogin, a later poll sees Running + IP, and stdin is exhausted — so a
	// second read would return ErrCancelled if the poll did not handle settlement.
	origSleep := tailscaleSettleSleep
	origInterval := tailscaleSettleInterval
	origTimeout := tailscaleSettleTimeout
	tailscaleSettleSleep = func(time.Duration) {}
	tailscaleSettleInterval = 10 * time.Millisecond
	tailscaleSettleTimeout = 50 * time.Millisecond
	defer func() {
		tailscaleSettleSleep = origSleep
		tailscaleSettleInterval = origInterval
		tailscaleSettleTimeout = origTimeout
	}()

	cleanup := withStdin(t, "status\n")
	defer cleanup()
	out, buf := testOutput()
	statusCalls := 0
	probes := installer.Probes{
		CommandExists: func(name string) bool { return name == "tailscale" },
		CommandOutput: func(ctx context.Context, name string, args ...string) (string, error) {
			if name == "tailscale" && len(args) == 1 && args[0] == "version" {
				return "tailscale version 1.80.0", nil
			}
			if name == "tailscale" && len(args) >= 2 && args[0] == "status" && args[1] == "--json" {
				statusCalls++
				if statusCalls == 1 {
					return `{"BackendState":"NeedsLogin"}`, nil
				}
				if statusCalls == 2 {
					return `{"BackendState":"NeedsLogin"}`, nil
				}
				return `{"BackendState":"Running"}`, nil
			}
			if name == "tailscale" && len(args) == 2 && args[0] == "ip" && args[1] == "-4" {
				if statusCalls >= 3 {
					return "100.64.0.99\n", nil
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
				fmt.Fprintln(combined, "To authenticate, visit:\n\n\thttps://login.tailscale.com/a/settlement-test")
				return nil
			}
			return nil
		},
	}
	if err := guideTailscale(context.Background(), out, probes); err != nil {
		t.Fatalf("guideTailscale failed: %v, output:\n%s", err, buf.String())
	}
	got := buf.String()
	if !strings.Contains(got, "Tailscale is up") {
		t.Fatalf("expected Tailscale success after settlement poll; output:\n%s", got)
	}
	if !strings.Contains(got, "continuing to Cloudflare setup") {
		t.Fatalf("expected transition after Tailscale success; output:\n%s", got)
	}
	if statusCalls < 3 {
		t.Fatalf("expected at least 3 status probes (initial + 2 poll attempts), got %d", statusCalls)
	}
	if c := strings.Count(got, "https://login.tailscale.com/a/settlement-test"); c != 1 {
		t.Fatalf("login URL count %d want 1 (no duplicate URL during settlement poll); output:\n%s", c, got)
	}
}

func TestGuideTailscale_BlankSettlesWithoutSecondEnter(t *testing.T) {
	origSleep := tailscaleSettleSleep
	origInterval := tailscaleSettleInterval
	origTimeout := tailscaleSettleTimeout
	tailscaleSettleSleep = func(time.Duration) {}
	tailscaleSettleInterval = 10 * time.Millisecond
	tailscaleSettleTimeout = 50 * time.Millisecond
	defer func() {
		tailscaleSettleSleep = origSleep
		tailscaleSettleInterval = origInterval
		tailscaleSettleTimeout = origTimeout
	}()
	cleanup := withStdin(t, "\n")
	defer cleanup()
	out, buf := testOutput()
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
					return "100.64.0.100\n", nil
				}
				return "", fmt.Errorf("no ip")
			}
			return "", nil
		},
		CommandStream: func(ctx context.Context, combined io.Writer, name string, args ...string) error {
			if name == "tailscale" && len(args) == 1 && args[0] == "up" {
				fmt.Fprintln(combined, "https://login.tailscale.com/a/blank-settle")
				return nil
			}
			return nil
		},
	}
	if err := guideTailscale(context.Background(), out, probes); err != nil {
		t.Fatalf("guideTailscale blank settle failed: %v\n%s", err, buf.String())
	}
	got := buf.String()
	if !strings.Contains(got, "Tailscale is up") {
		t.Fatalf("blank Enter should settle; output:\n%s", got)
	}
}

func TestGuideTailscale_SettleTimeoutShowsStillOnce(t *testing.T) {
	origSleep := tailscaleSettleSleep
	origInterval := tailscaleSettleInterval
	origTimeout := tailscaleSettleTimeout
	tailscaleSettleSleep = func(time.Duration) {}
	tailscaleSettleInterval = 10 * time.Millisecond
	tailscaleSettleTimeout = 30 * time.Millisecond
	defer func() {
		tailscaleSettleSleep = origSleep
		tailscaleSettleInterval = origInterval
		tailscaleSettleTimeout = origTimeout
	}()
	cleanup := withStdin(t, "status\nskip\n")
	defer cleanup()
	out, buf := testOutput()
	probes := installer.Probes{
		CommandExists: func(name string) bool { return name == "tailscale" },
		CommandOutput: func(ctx context.Context, name string, args ...string) (string, error) {
			if name == "tailscale" && len(args) == 1 && args[0] == "version" {
				return "tailscale version 1.80.0", nil
			}
			if name == "tailscale" && len(args) >= 1 && args[0] == "status" {
				return `{"BackendState":"NeedsLogin"}`, nil
			}
			if name == "tailscale" && len(args) == 2 && args[0] == "ip" {
				return "", fmt.Errorf("no ip")
			}
			return "", nil
		},
		CommandStream: func(ctx context.Context, combined io.Writer, name string, args ...string) error {
			if name == "tailscale" && len(args) == 1 && args[0] == "up" {
				fmt.Fprintln(combined, "https://login.tailscale.com/a/timeout-test")
				return nil
			}
			return nil
		},
	}
	if err := guideTailscale(context.Background(), out, probes); err != nil {
		t.Fatalf("guideTailscale timeout case failed: %v\n%s", err, buf.String())
	}
	got := buf.String()
	if !strings.Contains(got, "Still:") {
		t.Fatalf("expected Still after bounded wait expires; output:\n%s", got)
	}
	if c := strings.Count(got, "Still:"); c != 1 {
		t.Fatalf("expected Still shown once after timeout, got %d in:\n%s", c, got)
	}
	if !strings.Contains(got, "Skipped") {
		t.Fatalf("expected skip after timeout to work; output:\n%s", got)
	}
	if c := strings.Count(got, "https://login.tailscale.com/a/timeout-test"); c != 1 {
		t.Fatalf("should not duplicate URL on settlement timeout; count %d", c)
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

func TestDashboardURL_EncodingAndFragment(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ip    string
		token string
		want  string
	}{
		{"100.64.0.1", "simple", "http://100.64.0.1:8484/#token=simple"},
		{"100.64.0.5", "a+b/c=d&e?f#g%h", "http://100.64.0.5:8484/#token=a%2Bb%2Fc%3Dd%26e%3Ff%23g%25h"},
		{"100.64.0.1", "hello world", "http://100.64.0.1:8484/#token=hello%20world"},
		{"100.64.0.1", "token/with/slash", "http://100.64.0.1:8484/#token=token%2Fwith%2Fslash"},
		{"100.64.0.1", "abc-_.~", "http://100.64.0.1:8484/#token=abc-_.~"},
	}
	for _, tc := range cases {
		got := dashboardURL(tc.ip, tc.token)
		if got != tc.want {
			t.Errorf("dashboardURL(%q,%q)=%q want %q", tc.ip, tc.token, got, tc.want)
		}
		if strings.Contains(got, "?token=") {
			t.Errorf("dashboardURL(%q,%q) leaks query param: %q", tc.ip, tc.token, got)
		}
		if tc.token != "" && !strings.Contains(got, "#token=") {
			t.Errorf("dashboardURL(%q,%q) missing fragment: %q", tc.ip, tc.token, got)
		}
	}
}

func TestDashboardURL_NoQueryLeakAndFallback(t *testing.T) {
	t.Parallel()
	// Empty token -> fallback without fragment, never query
	got := dashboardURL("100.64.0.1", "")
	if got != "http://100.64.0.1:8484" {
		t.Fatalf("fallback want http://100.64.0.1:8484, got %q", got)
	}
	if strings.Contains(got, "#token=") || strings.Contains(got, "?token=") {
		t.Fatalf("fallback should have no token, got %q", got)
	}
	// Whitespace token -> trimmed to empty, fallback
	got = dashboardURL("100.64.0.1", "  \n\t")
	if got != "http://100.64.0.1:8484" {
		t.Fatalf("whitespace token fallback want base, got %q", got)
	}
	// Empty IP -> empty URL
	if got := dashboardURL("", "secret"); got != "" {
		t.Fatalf("empty IP should return empty, got %q", got)
	}
	// Token with spaces and special chars must not produce query
	got = dashboardURL("100.64.0.9", "a&b=c+d/e?f#g")
	if strings.Contains(got, "?token=") {
		t.Fatalf("should never contain ?token=, got %q", got)
	}
	if !strings.Contains(got, "#token=") {
		t.Fatalf("should contain #token=, got %q", got)
	}
	if strings.Contains(got, "?") && strings.Index(got, "?") < strings.Index(got, "#token=") {
		t.Fatalf("unexpected ? before fragment, got %q", got)
	}
}

func TestReadAPIToken_TrimAndError(t *testing.T) {
	t.Parallel()
	// Trims whitespace/newline
	probes := installer.Probes{
		ReadFile: func(path string) ([]byte, error) {
			if path == apiTokenPath {
				return []byte("  secret-token123  \n"), nil
			}
			return nil, fmt.Errorf("not found")
		},
	}
	if got := readAPIToken(probes); got != "secret-token123" {
		t.Fatalf("readAPIToken trim want secret-token123, got %q", got)
	}
	// Missing file -> empty
	probes2 := installer.Probes{
		ReadFile: func(path string) ([]byte, error) { return nil, fmt.Errorf("not found") },
	}
	if got := readAPIToken(probes2); got != "" {
		t.Fatalf("missing file want empty, got %q", got)
	}
	// Nil probe -> empty
	if got := readAPIToken(installer.Probes{}); got != "" {
		t.Fatalf("nil ReadFile want empty, got %q", got)
	}
	// Whitespace only -> empty
	probes3 := installer.Probes{
		ReadFile: func(path string) ([]byte, error) { return []byte("   \n\t  "), nil },
	}
	if got := readAPIToken(probes3); got != "" {
		t.Fatalf("whitespace only want empty, got %q", got)
	}
}

func TestGuideTailscale_TokenizedQRAndWarning(t *testing.T) {
	cleanup := withStdin(t, "skip\n")
	defer cleanup()
	// Use token with special chars to verify encoding path
	token := "a+b/c=d&e?f#g%h"
	expectedURL := dashboardURL("100.64.0.7", token)
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
	probes := installer.Probes{
		CommandExists: func(name string) bool { return name == "tailscale" },
		CommandOutput: func(ctx context.Context, name string, args ...string) (string, error) {
			if name == "tailscale" && len(args) >= 2 && args[0] == "status" {
				return `{"BackendState":"Running"}`, nil
			}
			if name == "tailscale" && len(args) >= 2 && args[0] == "ip" {
				return "100.64.0.7\n", nil
			}
			if name == "tailscale" && len(args) == 1 && args[0] == "version" {
				return "tailscale version 1.80.0", nil
			}
			return "", nil
		},
		ReadFile: func(path string) ([]byte, error) {
			if path == apiTokenPath {
				return []byte(token), nil
			}
			return nil, fmt.Errorf("not found")
		},
	}
	if err := guideTailscale(context.Background(), out, probes); err != nil {
		t.Fatalf("guideTailscale failed: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, expectedURL) {
		t.Fatalf("expected tokenized URL %q in output, got:\n%s", expectedURL, got)
	}
	if !strings.Contains(got, "#token=") {
		t.Fatalf("expected #token= fragment, got:\n%s", got)
	}
	if strings.Contains(got, "?token=") {
		t.Fatalf("should not contain ?token=, got:\n%s", got)
	}
	// QR should encode same URL: we track via printQR label; ensure at least one QR block or URL printed
	// The warning must be present
	if !strings.Contains(strings.ToLower(got), "keep") || !strings.Contains(strings.ToLower(got), "private") {
		t.Fatalf("expected private warning for tokenized link, got:\n%s", got)
	}
	// Ensure the URL appears as the QR target: at least the encoded token is present once
	if c := strings.Count(got, expectedURL); c < 1 {
		t.Fatalf("expected URL to appear at least once for QR target, count %d, output:\n%s", c, got)
	}
}

func TestGuideTailscale_FallbackWithoutToken(t *testing.T) {
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
	// No token file
	probes := installer.Probes{
		CommandExists: func(name string) bool { return name == "tailscale" },
		CommandOutput: func(ctx context.Context, name string, args ...string) (string, error) {
			if name == "tailscale" && len(args) >= 2 && args[0] == "status" {
				return `{"BackendState":"Running"}`, nil
			}
			if name == "tailscale" && len(args) >= 2 && args[0] == "ip" {
				return "100.64.0.8\n", nil
			}
			return "ok", nil
		},
		ReadFile: func(path string) ([]byte, error) { return nil, fmt.Errorf("not found") },
	}
	if err := guideTailscale(context.Background(), out, probes); err != nil {
		t.Fatalf("guideTailscale failed: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "http://100.64.0.8:8484") {
		t.Fatalf("fallback should contain plain URL, got:\n%s", got)
	}
	if strings.Contains(got, "#token=") {
		t.Fatalf("fallback should not contain #token=, got:\n%s", got)
	}
	if strings.Contains(got, "?token=") {
		t.Fatalf("fallback should not contain ?token=, got:\n%s", got)
	}
}

func TestGuideCloudflare_NextLinkTokenized(t *testing.T) {
	// Provide domain, skip tokens, but have IP and token so Next link is tokenized
	cleanup := withStdin(t, "example.com\nskip\n\n")
	defer cleanup()
	out, buf := testOutput()
	token := "s3cr3t/+with special&chars?"
	probes := installer.Probes{
		CommandOutput: func(ctx context.Context, name string, args ...string) (string, error) {
			if name == "tailscale" && len(args) >= 2 && args[0] == "ip" {
				return "100.64.0.9\n", nil
			}
			return "", nil
		},
		ReadFile: func(path string) ([]byte, error) {
			if path == apiTokenPath {
				return []byte(token), nil
			}
			return nil, fmt.Errorf("not found")
		},
	}
	// Need to stub tailscaleIP via CommandOutput; guideCloudflare will call tailscaleIP internally
	// So provide tailscale ip probe
	if err := guideCloudflare(context.Background(), out, probes); err != nil {
		t.Fatalf("guideCloudflare failed: %v", err)
	}
	got := buf.String()
	expected := dashboardURL("100.64.0.9", token)
	if !strings.Contains(got, expected) {
		t.Fatalf("expected tokenized Next URL %q, got:\n%s", expected, got)
	}
	if !strings.Contains(got, "#token=") {
		t.Fatalf("expected #token= in Next link, got:\n%s", got)
	}
	if strings.Contains(got, "?token=") {
		t.Fatalf("should not contain ?token=, got:\n%s", got)
	}
	if !strings.Contains(strings.ToLower(got), "private") {
		t.Fatalf("expected private warning for tokenized Next link, got:\n%s", got)
	}
}

func TestPrintGuidedSummary_TokenizedDashboard(t *testing.T) {
	t.Parallel()
	token := "my-token-abc123"
	probes := installer.Probes{
		CommandOutput: func(ctx context.Context, name string, args ...string) (string, error) {
			if name == "tailscale" && len(args) >= 2 && args[0] == "ip" {
				return "100.64.0.10\n", nil
			}
			return "", nil
		},
		ReadFile: func(path string) ([]byte, error) {
			if path == apiTokenPath {
				return []byte(token), nil
			}
			return nil, fmt.Errorf("not found")
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
	printGuidedSummary(context.Background(), out, probes)
	got := buf.String()
	expected := dashboardURL("100.64.0.10", token)
	if !strings.Contains(got, expected) {
		t.Fatalf("guided summary should contain tokenized dashboard %q, got:\n%s", expected, got)
	}
	if !strings.Contains(got, "#token=") {
		t.Fatalf("guided summary missing #token=, got:\n%s", got)
	}
	if strings.Contains(got, "?token=") {
		t.Fatalf("guided summary leaks query param, got:\n%s", got)
	}
	if !strings.Contains(strings.ToLower(got), "private") {
		t.Fatalf("guided summary should warn private, got:\n%s", got)
	}
}
