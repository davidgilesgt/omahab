package tui

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/installer"
)

// TestChecklistGolden verifies PreflightChecklist renders spinner, chips, and freezes.
func TestChecklistGolden(t *testing.T) {
	caps := Caps{IsTTY: true, ColorEnabled: false, Width: 80} // plain for deterministic golden (no ANSI)
	m := NewPreflightChecklist(caps)
	// Simulate StepStarted preflight
	m.Feed(installer.StepStarted{Step: installer.StepPreflight})
	if got := m.View(); !strings.Contains(got, "running") {
		t.Fatalf("expected spinner running line before checks, got %q", got)
	}
	// Feed two checks: pass and fail
	m.Feed(installer.PreflightCheck{Result: installer.CheckResult{Name: "os", Level: installer.LevelPass, Message: "Debian 13"}})
	m.Feed(installer.PreflightCheck{Result: installer.CheckResult{Name: "ram", Level: installer.LevelFail, Message: "too little", Remediation: "add RAM"}})
	// View should contain both chips and spinner still (since not frozen)
	view := m.View()
	if !strings.Contains(view, "os") || !strings.Contains(view, "ram") {
		t.Fatalf("view missing checks: %q", view)
	}
	if !strings.Contains(view, "PASS") || !strings.Contains(view, "FAIL") {
		t.Fatalf("chips missing PASS/FAIL: %q", view)
	}
	// Spinner line should still be present (running preflight) until frozen
	if !strings.Contains(view, "running") {
		t.Fatalf("expected spinner still, got %q", view)
	}
	// Freeze (StepFinished)
	m.Feed(installer.StepFinished{Result: installer.RunResult{Step: installer.StepPreflight, Status: installer.JournalCompleted}})
	frozen := m.View()
	if strings.Contains(frozen, "running") {
		t.Fatalf("frozen frame should not contain spinner, got %q", frozen)
	}
	if !strings.Contains(frozen, "os") || !strings.Contains(frozen, "ram") {
		t.Fatalf("frozen missing checks")
	}
	// Golden file compare (plain)
	golden := "  [x] PASS os               Debian 13\n  [!] FAIL ram              too little\n       -> add RAM\n"
	if frozen != golden {
		t.Fatalf("checklist golden mismatch:\ngot:\n%q\nwant:\n%q", frozen, golden)
	}
}

// TestChecklistGlyphFallback ensures ●○✓ fallback to [x] [ ] in dumb mode.
func TestChecklistGlyphFallback(t *testing.T) {
	capsTty := Caps{IsTTY: true, ColorEnabled: true, Width: 80}
	capsDumb := Caps{IsTTY: false, ColorEnabled: false, Width: 80}
	if GlyphPass(capsTty) != "●" {
		t.Fatalf("tty glyph pass should be ●, got %q", GlyphPass(capsTty))
	}
	if GlyphPass(capsDumb) != "[x]" {
		t.Fatalf("dumb glyph pass should be [x], got %q", GlyphPass(capsDumb))
	}
	if GlyphStepDone(capsTty) != "✓" {
		t.Fatalf("tty step done should be ✓")
	}
	if GlyphStepDone(capsDumb) != "[x]" {
		t.Fatalf("dumb step done fallback")
	}
	if GlyphStepPending(capsDumb) != "[ ]" {
		t.Fatalf("pending fallback")
	}
}

// TestStepBarGolden validates StepBar renders completed/running/pending with separator.
func TestStepBarGolden(t *testing.T) {
	caps := Caps{IsTTY: false, ColorEnabled: false, Width: 80}
	steps := []string{"ssh_keys", "sshd_hardening", "packages"}
	sb := NewStepBar(caps, steps)
	// Fix time for deterministic elapsed
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	sb.SetNow(func() time.Time { return base.Add(14 * time.Second) })
	// Simulate: ssh_keys completed 2s, sshd running 14s, packages pending
	sb.LoadJournal([]installer.JournalEntry{
		{Step: "ssh_keys", Status: installer.JournalCompleted, StartedAt: ptrTime(base), FinishedAt: ptrTime(base.Add(2 * time.Second))},
		{Step: "sshd_hardening", Status: installer.JournalRunning, StartedAt: ptrTime(base)},
	})
	// pending packages remains
	view := sb.View()
	// Expect ssh_keys [x] with 2s, sshd [o] with 14s, packages [ ]
	if !strings.Contains(view, "ssh_keys") || !strings.Contains(view, "[x]") {
		t.Fatalf("ssh_keys should be done [x], got %q", view)
	}
	if !strings.Contains(view, "sshd_hardening") || !strings.Contains(view, "[o]") {
		t.Fatalf("sshd should be running [o], got %q", view)
	}
	if !strings.Contains(view, "packages") || !strings.Contains(view, "[ ]") {
		t.Fatalf("packages pending [ ], got %q", view)
	}
	if !strings.Contains(view, "·") {
		t.Fatalf("separator · missing, got %q", view)
	}
	// Golden file style check: should be exactly "ssh_keys [x] 2s · sshd_hardening [o] 14s · packages [ ]"
	expected := "ssh_keys [x] 2s · sshd_hardening [o] 14s · packages [ ]"
	if view != expected {
		t.Fatalf("stepbar golden mismatch:\ngot: %q\nwant: %q", view, expected)
	}
}

// TestStepBarResumePreCheck ensures --resume renders completed steps pre-checked from journal then follows live.
func TestStepBarResumePreCheck(t *testing.T) {
	caps := Caps{IsTTY: false, ColorEnabled: false, Width: 80}
	sb := NewStepBar(caps, nil) // uses OrderedSteps
	// Simulate journal with first 3 steps completed (resume)
	now := time.Now()
	entries := []installer.JournalEntry{
		{Step: installer.StepPreflight, Status: installer.JournalCompleted, StartedAt: ptrTime(now.Add(-30 * time.Second)), FinishedAt: ptrTime(now.Add(-28 * time.Second))},
		{Step: installer.StepSSHKeys, Status: installer.JournalCompleted, StartedAt: ptrTime(now.Add(-28 * time.Second)), FinishedAt: ptrTime(now.Add(-27 * time.Second))},
		{Step: installer.StepSSHDHardening, Status: installer.JournalCompleted, StartedAt: ptrTime(now.Add(-27 * time.Second)), FinishedAt: ptrTime(now.Add(-26 * time.Second))},
		{Step: installer.StepSystemPrepare, Status: installer.JournalPending},
		{Step: installer.StepPackages, Status: installer.JournalPending},
	}
	sb.LoadJournal(entries)
	view := sb.View()
	// Completed steps should be [x] pre-checked
	for _, s := range []string{installer.StepPreflight, installer.StepSSHKeys, installer.StepSSHDHardening} {
		if !strings.Contains(view, s+" [x]") {
			t.Fatalf("resume pre-check missing completed %s as [x] in %q", s, view)
		}
	}
	// Then simulate live StepStarted for packages
	sb.Feed(installer.StepStarted{Step: installer.StepPackages})
	view2 := sb.View()
	if !strings.Contains(view2, "packages [o]") {
		t.Fatalf("after StepStarted packages should be running [o], got %q", view2)
	}
	// StepFinished for packages should become completed
	sb.Feed(installer.StepFinished{Result: installer.RunResult{Step: installer.StepPackages, Status: installer.JournalCompleted}})
	view3 := sb.View()
	if !strings.Contains(view3, "packages [x]") {
		t.Fatalf("after StepFinished packages should be [x], got %q", view3)
	}
}

// TestReceiptWidth ensures receipt ≤64 columns and contains required fields.
func TestReceiptWidth(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	m := installer.Manifest{
		Version:     "0.1.0",
		InstalledAt: now,
		Arch:        "amd64",
		OS:          installer.OSInfo{ID: "debian", VersionID: "13", Pretty: "Debian GNU/Linux 13 (trixie)"},
		Steps: []installer.JournalEntry{
			{Step: installer.StepPreflight, Status: installer.JournalCompleted, StartedAt: ptrTime(now), FinishedAt: ptrTime(now.Add(2 * time.Second))},
			{Step: installer.StepSSHKeys, Status: installer.JournalCompleted, StartedAt: ptrTime(now.Add(2 * time.Second)), FinishedAt: ptrTime(now.Add(3 * time.Second))},
			{Step: installer.StepPackages, Status: installer.JournalCompleted, StartedAt: ptrTime(now.Add(3 * time.Second)), FinishedAt: ptrTime(now.Add(10 * time.Second))},
		},
	}
	receipt := RenderReceipt(m)
	lines := strings.Split(strings.TrimRight(receipt, "\n"), "\n")
	for i, line := range lines {
		if len(line) > 64 {
			t.Fatalf("line %d exceeds 64 cols (%d): %q", i, len(line), line)
		}
	}
	// Must contain host, version, step durations, next actions
	if !strings.Contains(receipt, "host:") {
		t.Fatalf("receipt missing host")
	}
	if !strings.Contains(receipt, "version:") || !strings.Contains(receipt, "0.1.0") {
		t.Fatalf("receipt missing version")
	}
	if !strings.Contains(receipt, "preflight") {
		t.Fatalf("receipt missing steps")
	}
	if !strings.Contains(receipt, "omahab doctor") {
		t.Fatalf("receipt missing next action omahab doctor")
	}
	if !strings.Contains(receipt, "dashboard:") {
		t.Fatalf("receipt missing dashboard URL")
	}
	if !strings.Contains(receipt, "tailscale:") {
		t.Fatalf("receipt missing tailscale URL")
	}
	// Also check with tailscale IP
	receipt2 := RenderReceiptWithCaps(m, Caps{Width: 64}, "100.64.0.1")
	if !strings.Contains(receipt2, "100.64.0.1") {
		t.Fatalf("receipt with IP missing dashboard IP, got %q", receipt2)
	}
	// Ensure plain caps (non-TTY) still ≤64
	for _, line := range strings.Split(receipt2, "\n") {
		if len(line) > 64 {
			t.Fatalf("receipt2 line exceeds 64: %q", line)
		}
	}
}

// TestSecondSessionGateCountdown checks live countdown polling.
func TestSecondSessionGateCountdown(t *testing.T) {
	caps := Caps{IsTTY: false, ColorEnabled: false, Width: 80}
	start := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	gate := NewSecondSessionGateAt(caps, start)
	gate.SetNow(func() time.Time { return start.Add(30 * time.Second) })
	rem := gate.Remaining()
	if rem != 9*time.Minute+30*time.Second {
		t.Fatalf("remaining should be 9m30s, got %v", rem)
	}
	if gate.IsExpired() {
		t.Fatalf("should not be expired")
	}
	// After 10m should be expired
	gate.SetNow(func() time.Time { return start.Add(10 * time.Minute).Add(time.Second) })
	if !gate.IsExpired() {
		t.Fatalf("should be expired after 10m")
	}
	if gate.Remaining() != 0 {
		t.Fatalf("remaining after expired should be 0, got %v", gate.Remaining())
	}
	view := gate.View()
	if !strings.Contains(view, "rollback") {
		t.Fatalf("gate view should contain rollback, got %q", view)
	}
}

// TestStylesAccent ensures accent tokens match web/src/styles.css exactly.
func TestStylesAccent(t *testing.T) {
	if Accent.Light != "#4C5B36" {
		t.Fatalf("Accent Light mismatch: got %q want #4C5B36", Accent.Light)
	}
	if Accent.Dark != "#B2C27D" {
		t.Fatalf("Accent Dark mismatch: got %q want #B2C27D", Accent.Dark)
	}
	// Check positive/warning/negative match web token (spot check)
	if PositiveFG.Light != "#35613f" || PositiveBG.Light != "#dfeade" {
		t.Fatalf("positive palette mismatch")
	}
	if WarningFG.Light != "#765b14" || NegativeFG.Light != "#8b332b" {
		t.Fatalf("warning/negative mismatch")
	}
}

func TestStepBarCompactWidthSafe(t *testing.T) {
	// Non-TTY full view should remain unwrapped for log-friendly transcripts.
	capsPlain := Caps{IsTTY: false, ColorEnabled: false, Width: 80}
	sbPlain := NewStepBar(capsPlain, nil)
	sbPlain.LoadJournal([]installer.JournalEntry{
		{Step: installer.StepPreflight, Status: installer.JournalCompleted, StartedAt: ptrTime(time.Now().Add(-30 * time.Second)), FinishedAt: ptrTime(time.Now().Add(-28 * time.Second))},
		{Step: installer.StepSSHKeys, Status: installer.JournalCompleted, StartedAt: ptrTime(time.Now().Add(-28 * time.Second)), FinishedAt: ptrTime(time.Now().Add(-27 * time.Second))},
		{Step: installer.StepSSHDHardening, Status: installer.JournalCompleted, StartedAt: ptrTime(time.Now().Add(-27 * time.Second)), FinishedAt: ptrTime(time.Now().Add(-26 * time.Second))},
		{Step: installer.StepSystemPrepare, Status: installer.JournalCompleted, StartedAt: ptrTime(time.Now().Add(-26 * time.Second)), FinishedAt: ptrTime(time.Now().Add(-25 * time.Second))},
		{Step: installer.StepPackages, Status: installer.JournalCompleted, StartedAt: ptrTime(time.Now().Add(-25 * time.Second)), FinishedAt: ptrTime(time.Now().Add(-20 * time.Second))},
		{Step: installer.StepBinaries, Status: installer.JournalCompleted, StartedAt: ptrTime(time.Now().Add(-20 * time.Second)), FinishedAt: ptrTime(time.Now().Add(-15 * time.Second))},
		{Step: installer.StepFirewall, Status: installer.JournalCompleted, StartedAt: ptrTime(time.Now().Add(-15 * time.Second)), FinishedAt: ptrTime(time.Now().Add(-14 * time.Second))},
		{Step: installer.StepServices, Status: installer.JournalRunning, StartedAt: ptrTime(time.Now())},
	})
	plainView := sbPlain.View()
	if len(plainView) <= 80 {
		t.Fatalf("plain full view expected to exceed 80 for 11 steps, got %d: %q", len(plainView), plainView)
	}
	// TTY view must be width-safe single line via compact.
	capsTTY := Caps{IsTTY: true, ColorEnabled: true, Width: 80}
	sbTTY := NewStepBar(capsTTY, nil)
	// Copy state from plain
	sbTTY.LoadJournal([]installer.JournalEntry{
		{Step: installer.StepPreflight, Status: installer.JournalCompleted, StartedAt: ptrTime(time.Now().Add(-30 * time.Second)), FinishedAt: ptrTime(time.Now().Add(-28 * time.Second))},
		{Step: installer.StepSSHKeys, Status: installer.JournalCompleted, StartedAt: ptrTime(time.Now().Add(-28 * time.Second)), FinishedAt: ptrTime(time.Now().Add(-27 * time.Second))},
		{Step: installer.StepSSHDHardening, Status: installer.JournalCompleted, StartedAt: ptrTime(time.Now().Add(-27 * time.Second)), FinishedAt: ptrTime(time.Now().Add(-26 * time.Second))},
		{Step: installer.StepSystemPrepare, Status: installer.JournalCompleted, StartedAt: ptrTime(time.Now().Add(-26 * time.Second)), FinishedAt: ptrTime(time.Now().Add(-25 * time.Second))},
		{Step: installer.StepPackages, Status: installer.JournalCompleted, StartedAt: ptrTime(time.Now().Add(-25 * time.Second)), FinishedAt: ptrTime(time.Now().Add(-20 * time.Second))},
		{Step: installer.StepBinaries, Status: installer.JournalCompleted, StartedAt: ptrTime(time.Now().Add(-20 * time.Second)), FinishedAt: ptrTime(time.Now().Add(-15 * time.Second))},
		{Step: installer.StepFirewall, Status: installer.JournalCompleted, StartedAt: ptrTime(time.Now().Add(-15 * time.Second)), FinishedAt: ptrTime(time.Now().Add(-14 * time.Second))},
		{Step: installer.StepServices, Status: installer.JournalRunning, StartedAt: ptrTime(time.Now())},
	})
	ttyView := sbTTY.View()
	if len(ttyView) > 80 {
		t.Fatalf("TTY compact view must fit within 80, got %d: %q", len(ttyView), ttyView)
	}
	if strings.Contains(ttyView, "\n") {
		t.Fatalf("TTY compact view must be single line, got %q", ttyView)
	}
	// Plain should contain all steps, TTY compact may use ellipsis but must still show running step.
	if !strings.Contains(plainView, "preflight") || !strings.Contains(plainView, "manifest") {
		t.Fatalf("plain view missing steps: %q", plainView)
	}
	if !strings.Contains(ttyView, "services") {
		t.Fatalf("compact view should preserve running step, got %q", ttyView)
	}
	// Compact must not duplicate neighbor parts at boundaries (lo==1 and hi==len-2).
	for _, step := range installer.OrderedSteps {
		if c := strings.Count(ttyView, step); c > 1 {
			t.Fatalf("compact view duplicated step %q %d times, got %q", step, c, ttyView)
		}
	}
	// Explicit narrow boundary cases: running at index 1 (lo==1) and at len-2 (hi==len-2).
	narrowCaps := Caps{IsTTY: true, ColorEnabled: true, Width: 40}
	sbN1 := NewStepBar(narrowCaps, []string{"a", "b", "c", "d", "e", "f", "g"})
	sbN1.Feed(installer.StepStarted{Step: "b"})
	v1 := sbN1.View()
	if strings.Count(v1, "b") != 1 || strings.Count(v1, "a") != 1 {
		t.Fatalf("narrow lo==1 duplicated, got %q", v1)
	}
	sbN2 := NewStepBar(narrowCaps, []string{"a", "b", "c", "d", "e", "f", "g"})
	sbN2.Feed(installer.StepStarted{Step: "e"})
	v2 := sbN2.View()
	if strings.Count(v2, "e") != 1 || strings.Count(v2, "g") != 1 {
		t.Fatalf("narrow hi==len-2 duplicated, got %q", v2)
	}
}
func TestTUIRendererPinnedFrame(t *testing.T) {
	capsTTY := Caps{IsTTY: true, ColorEnabled: true, Width: 80}
	var buf bytes.Buffer
	r := NewTUIRenderer(&buf, capsTTY)
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	r.StepBar().SetNow(func() time.Time { return base })
	// Initial StepStarted should render pinned frame in-place (no newline, uses \r)
	r.Render(installer.StepStarted{Step: installer.StepSSHKeys})
	first := buf.String()
	if !strings.Contains(first, "\r") {
		t.Fatalf("TTY pinned frame should use carriage return, got %q", first)
	}
	if strings.Count(first, "\n") != 0 {
		t.Fatalf("TTY pinned frame should not append newline on start, got %q", first)
	}
	if !strings.Contains(first, "ssh_keys") {
		t.Fatalf("frame missing step name, got %q", first)
	}
	buf.Reset()
	// StepLog is detail: must clear frame before printing, preserve line, and re-render frame so progress survives long logs
	r.Render(installer.StepLog{Step: installer.StepSSHKeys, Line: "downloading packages..."})
	second := buf.String()
	if !strings.Contains(second, "\033[K") {
		t.Fatalf("detail should clear pinned frame with \\033[K, got %q", second)
	}
	if !strings.Contains(second, "downloading") {
		t.Fatalf("detail line must remain readable, got %q", second)
	}
	// Frame must survive the log: after the log line the bar is re-pinned in-place
	if !strings.Contains(second, "ssh_keys") {
		t.Fatalf("frame should survive StepLog and be re-rendered, got %q", second)
	}
	if strings.Count(second, "\r") < 2 {
		t.Fatalf("StepLog should clear and re-render frame (at least 2 \\r), got %q", second)
	}
	// After detail, frame is re-pinned, next progress should still be in-place
	buf.Reset()
	r.Render(installer.StepFinished{Result: installer.RunResult{Step: installer.StepSSHKeys, Status: installer.JournalCompleted}})
	third := buf.String()
	if !strings.Contains(third, "[ok]") {
		t.Fatalf("finish should contain [ok], got %q", third)
	}
	if !strings.Contains(third, "\r") {
		t.Fatalf("finish should re-render pinned frame in-place, got %q", third)
	}
	if strings.Count(third, "ssh_keys") > 2 {
		t.Fatalf("should not duplicate frame lines via Fprintln, got %q", third)
	}
	buf.Reset()
	r.Finalize()
	final := buf.String()
	if final != "\n" {
		t.Fatalf("Finalize must commit frame with single newline, got %q", final)
	}
}

func TestTUIRendererPlainAppendOnly(t *testing.T) {
	capsPlain := Caps{IsTTY: false, ColorEnabled: false, Width: 80}
	var buf bytes.Buffer
	r := NewTUIRenderer(&buf, capsPlain)
	r.StepBar().SetNow(func() time.Time { return time.Now() })
	r.Render(installer.StepStarted{Step: installer.StepSSHKeys})
	first := buf.String()
	if strings.Contains(first, "\r") {
		t.Fatalf("plain (non-TTY) should not use in-place \\r, got %q", first)
	}
	if !strings.Contains(first, "\n") {
		t.Fatalf("plain should append with newline, got %q", first)
	}
	buf.Reset()
	r.Render(installer.StepLog{Step: installer.StepSSHKeys, Line: "plain log line"})
	second := buf.String()
	if strings.Contains(second, "\033[K") {
		t.Fatalf("plain should not clear frame, got %q", second)
	}
	if !strings.Contains(second, "plain log line") {
		t.Fatalf("plain detail missing, got %q", second)
	}
}

// Golden-file tests against canned event streams (files in testdata)
func TestGoldenFiles(t *testing.T) {
	// checklist golden
	caps := Caps{IsTTY: true, ColorEnabled: false, Width: 80}
	cl := NewPreflightChecklist(caps)
	cl.Feed(installer.PreflightCheck{Result: installer.CheckResult{Name: "os", Level: installer.LevelPass, Message: "Debian 13"}})
	cl.Feed(installer.PreflightCheck{Result: installer.CheckResult{Name: "ram", Level: installer.LevelFail, Message: "too little", Remediation: "add RAM"}})
	cl.Freeze()
	checkGot := cl.View()
	checkWantB, err := os.ReadFile("testdata/checklist.golden")
	if err != nil {
		t.Fatalf("read checklist golden: %v", err)
	}
	if checkGot != string(checkWantB) {
		t.Fatalf("checklist golden file mismatch:\n got %q\n want %q", checkGot, string(checkWantB))
	}
	// stepbar golden
	caps2 := Caps{IsTTY: false, ColorEnabled: false, Width: 80}
	steps := []string{"ssh_keys", "sshd_hardening", "packages"}
	sb := NewStepBar(caps2, steps)
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	sb.SetNow(func() time.Time { return base.Add(14 * time.Second) })
	sb.LoadJournal([]installer.JournalEntry{
		{Step: "ssh_keys", Status: installer.JournalCompleted, StartedAt: ptrTime(base), FinishedAt: ptrTime(base.Add(2 * time.Second))},
		{Step: "sshd_hardening", Status: installer.JournalRunning, StartedAt: ptrTime(base)},
	})
	stepGot := sb.View()
	stepWantB, err := os.ReadFile("testdata/stepbar.golden")
	if err != nil {
		t.Fatalf("read stepbar golden: %v", err)
	}
	if stepGot != strings.TrimSpace(string(stepWantB)) && stepGot != string(stepWantB) {
		if strings.TrimSpace(stepGot) != strings.TrimSpace(string(stepWantB)) {
			t.Fatalf("stepbar golden mismatch:\n got %q\n want %q", stepGot, string(stepWantB))
		}
	}
	// receipt golden — width ≤64 already tested, here check file match
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	m := installer.Manifest{
		Version:     "0.1.0",
		InstalledAt: now,
		Arch:        "amd64",
		OS:          installer.OSInfo{ID: "debian", VersionID: "13", Pretty: "Debian GNU/Linux 13 (trixie)"},
		Steps: []installer.JournalEntry{
			{Step: installer.StepPreflight, Status: installer.JournalCompleted, StartedAt: ptrTime(now), FinishedAt: ptrTime(now.Add(2 * time.Second))},
			{Step: installer.StepSSHKeys, Status: installer.JournalCompleted, StartedAt: ptrTime(now.Add(2 * time.Second)), FinishedAt: ptrTime(now.Add(3 * time.Second))},
			{Step: installer.StepPackages, Status: installer.JournalCompleted, StartedAt: ptrTime(now.Add(3 * time.Second)), FinishedAt: ptrTime(now.Add(10 * time.Second))},
		},
	}
	receiptGot := RenderReceipt(m)
	receiptWantB, err := os.ReadFile("testdata/receipt.golden")
	if err != nil {
		t.Fatalf("read receipt golden: %v", err)
	}
	if receiptGot != string(receiptWantB) {
		t.Fatalf("receipt golden mismatch:\n got %q\n want %q", receiptGot, string(receiptWantB))
	}
	// resume golden
	sb2 := NewStepBar(caps2, nil)
	n := time.Now()
	resumeWantB, err := os.ReadFile("testdata/resume.golden")
	if err != nil {
		t.Fatalf("read resume golden: %v", err)
	}
	resumeContent := string(resumeWantB)
	if !strings.Contains(resumeContent, "preflight") || !strings.Contains(resumeContent, "[x]") {
		t.Fatalf("resume golden seems invalid: %q", resumeContent)
	}
	sb2.LoadJournal([]installer.JournalEntry{
		{Step: installer.StepPreflight, Status: installer.JournalCompleted, StartedAt: ptrTime(n.Add(-30 * time.Second)), FinishedAt: ptrTime(n.Add(-28 * time.Second))},
		{Step: installer.StepSSHKeys, Status: installer.JournalCompleted, StartedAt: ptrTime(n.Add(-28 * time.Second)), FinishedAt: ptrTime(n.Add(-27 * time.Second))},
	})
	_ = sb2
}

func ptrTime(t time.Time) *time.Time { return &t }
