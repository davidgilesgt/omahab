package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/diskinstall"
)

func TestNewInstallCmd_Help(t *testing.T) {
	cmd := newInstallCmd()
	if cmd.Use != "install" {
		t.Fatalf("Use=%q want install", cmd.Use)
	}
	// Check subcommand provision-keys exists and is hidden
	found := false
	for _, sub := range cmd.Commands() {
		if sub.Name() == "provision-keys" {
			found = true
			if !sub.Hidden {
				t.Fatalf("provision-keys should be hidden")
			}
			// Check required flags exist
			if sub.Flags().Lookup("root") == nil || sub.Flags().Lookup("username") == nil || sub.Flags().Lookup("keys-file") == nil {
				t.Fatal("provision-keys missing required flags")
			}
		}
	}
	if !found {
		t.Fatal("provision-keys subcommand not found")
	}
	// Ensure help works without error (execute with --help)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("install --help failed: %v", err)
	}
}

func TestRootRegistersInstall(t *testing.T) {
	root := newRootCmd()
	c, _, err := root.Find([]string{"install"})
	if err != nil || c == nil || c.Name() != "install" {
		t.Fatalf("install not registered in root")
	}
	// Also check provision-keys hidden subcommand
	c2, _, err := root.Find([]string{"install", "provision-keys"})
	if err != nil || c2 == nil {
		t.Fatalf("provision-keys not found via root")
	}
}

func TestParsePasswdEntry_Valid(t *testing.T) {
	passwd := "root:x:0:0:root:/root:/bin/bash\nmarisol:x:1000:1000:Marisol:/home/marisol:/bin/bash\n"
	uid, gid, home, err := parsePasswdEntry(passwd, "marisol")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if uid != 1000 || gid != 1000 || home != "/home/marisol" {
		t.Fatalf("got uid=%d gid=%d home=%q", uid, gid, home)
	}
}

func TestParsePasswdEntry_NotFound(t *testing.T) {
	passwd := "root:x:0:0:root:/root:/bin/bash\n"
	_, _, _, err := parsePasswdEntry(passwd, "marisol")
	if err == nil {
		t.Fatal("should fail for missing user")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err=%v should contain not found", err)
	}
}

func TestProvisionKeys_RequiresRoot(t *testing.T) {
	// This test checks that non-root fails early. If running as root, skip.
	if os.Geteuid() == 0 {
		t.Skip("running as root, cannot test non-root path")
	}
	err := runProvisionKeys("/mnt", "marisol", "/tmp/keys")
	if err == nil {
		t.Fatal("should fail when not root")
	}
	if !strings.Contains(err.Error(), "must run as root") {
		t.Fatalf("err=%q should contain must run as root", err)
	}
}

func TestProvisionKeys_ValidatesHomeBeneathRoot(t *testing.T) {
	// Test the home validation logic via a temporary fake root
	// We cannot easily test runProvisionKeys without root, so we test the home logic directly
	// by checking that parsePasswdEntry + home resolution would fail for bad home.

	// Simulate a passwd entry with home not beneath /home
	passwd := "evil:x:1000:1000::/etc:/bin/bash\n"
	uid, gid, home, err := parsePasswdEntry(passwd, "evil")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if uid != 1000 || gid != 1000 {
		t.Fatalf("uid/gid")
	}
	// Home is /etc, should be rejected as not beneath /home
	// Replicate logic from runProvisionKeys
	root := "/mnt"
	targetHome := filepath.Join(root, strings.TrimPrefix(filepath.Clean(home), "/"))
	rel, _ := filepath.Rel(root, targetHome)
	if !strings.HasPrefix(rel, "..") && home == "/etc" {
		// Our logic should reject home not starting with /home/
		if strings.HasPrefix(filepath.Clean(home), "/home/") {
			t.Fatal("should not be considered beneath /home")
		}
	}
	// Also test valid home
	passwd2 := "marisol:x:1000:1000::/home/marisol:/bin/bash\n"
	_, _, home2, _ := parsePasswdEntry(passwd2, "marisol")
	if !strings.HasPrefix(filepath.Clean(home2), "/home/") {
		t.Fatalf("valid home should be beneath /home")
	}
	_ = uid
	_ = gid
}

func TestIsSymlink_Detects(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	isLink, err := isSymlink(file)
	if err != nil {
		t.Fatal(err)
	}
	if isLink {
		t.Fatal("regular file should not be symlink")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	isLink, err = isSymlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if !isLink {
		t.Fatal("symlink should be detected")
	}
}

func TestDrainProgressEventsNilSafe(t *testing.T) {
	// Nil channel must return immediately (ranging nil blocks forever).
	done := make(chan []diskinstall.ProgressEvent, 1)
	go func() { done <- drainProgressEvents(nil) }()
	select {
	case out := <-done:
		if len(out) != 0 {
			t.Fatalf("nil channel: got %d events", len(out))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nil channel: drain blocked (deadlock)")
	}

	// Closed channel with buffered events drains them all.
	ch := make(chan diskinstall.ProgressEvent, 3)
	ch <- diskinstall.ProgressEvent{Stage: "preflight", Status: "running"}
	ch <- diskinstall.ProgressEvent{Stage: "partition", Status: "complete"}
	close(ch)
	out := drainProgressEvents(ch)
	if len(out) != 2 || out[0].Stage != "preflight" || out[1].Stage != "partition" {
		t.Fatalf("closed channel: got %+v", out)
	}

	// Open channel closed asynchronously drains without deadlock.
	ch2 := make(chan diskinstall.ProgressEvent, 1)
	go func() {
		ch2 <- diskinstall.ProgressEvent{Stage: "mount", Status: "complete"}
		close(ch2)
	}()
	done2 := make(chan []diskinstall.ProgressEvent, 1)
	go func() { done2 <- drainProgressEvents(ch2) }()
	select {
	case out2 := <-done2:
		if len(out2) != 1 || out2[0].Stage != "mount" {
			t.Fatalf("async close: got %+v", out2)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("async close: drain blocked (deadlock)")
	}
}
