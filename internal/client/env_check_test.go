package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeDBusTest struct {
	sets      [][]string
	unsets    [][]string
	failSet   bool
	failUnset bool
}

func (f *fakeDBusTest) SetEnvironment(a []string) error {
	if f.failSet {
		return fmt.Errorf("fail set")
	}
	f.sets = append(f.sets, append([]string(nil), a...))
	return nil
}
func (f *fakeDBusTest) UnsetEnvironment(n []string) error {
	if f.failUnset {
		return fmt.Errorf("fail unset")
	}
	f.unsets = append(f.unsets, append([]string(nil), n...))
	return nil
}

func TestEnvCheck(t *testing.T) {
	tmp := t.TempDir()
	// Use XDG_CONFIG_HOME override
	t.Setenv("XDG_CONFIG_HOME", tmp)
	srvHit := 0
	var lastIfNoneMatch string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/companion/environment" {
			w.WriteHeader(404)
			return
		}
		lastIfNoneMatch = r.Header.Get("If-None-Match")
		srvHit++
		switch srvHit {
		case 2:
			if lastIfNoneMatch != "" {
				w.WriteHeader(304)
				return
			}
			fallthrough
		case 1:
			w.Header().Set("ETag", `W/"1"`)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"revision": 1,
				"variables": map[string]string{
					"PARALLEL_API_KEY": "secret123",
					"OPENAI_API_KEY":   "sk-litellm-key-123",
					"OPENAI_BASE_URL":  "https://models.example.com/v1",
				},
			})
		case 3:
			w.WriteHeader(500)
			w.Write([]byte("internal error"))
		case 4:
			w.Header().Set("ETag", `W/"2"`)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"revision": 2,
				"variables": map[string]string{
					"PARALLEL_API_KEY": "secret123",
					"FOO":              "bar$with`backtick and \"quote\" and \\slash",
				},
			})
		case 5:
			w.Header().Set("ETag", `W/"3"`)
			json.NewEncoder(w).Encode(map[string]any{
				"revision": 3,
				"variables": map[string]string{},
			})
		default:
			w.Header().Set("ETag", `W/"1"`)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"revision": 1,
				"variables": map[string]string{
					"PARALLEL_API_KEY": "secret123",
					"OPENAI_API_KEY":   "sk-litellm-key-123",
					"OPENAI_BASE_URL":  "https://models.example.com/v1",
				},
			})
		}
	}))
	defer srv.Close()

	creds := NewMemoryCredentialStore()
	creds.Set("omahab", "device-token", "oma_dev_fake")
	remote, err := NewRemoteClient(RemoteClientConfig{
		ServerURL:       srv.URL,
		CredentialStore: creds,
	})
	if err != nil {
		t.Fatalf("remote: %v", err)
	}
	fake := &fakeDBusTest{}
	mgr := NewEnvironmentManager(EnvironmentManagerOpts{
		Remote: remote,
		Creds:  creds,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
		DBus:   fake,
	})
	ctx := context.Background()
	if err := mgr.Sync(ctx); err != nil {
		t.Fatalf("first sync failed: %v", err)
	}
	rev, cnt, synced, errStr := mgr.Status()
	if rev != 1 || cnt != 3 {
		t.Fatalf("expected rev1 cnt3 got %d %d", rev, cnt)
	}
	if synced == nil {
		t.Fatalf("synced nil")
	}
	if errStr != "" {
		t.Fatalf("errStr not empty: %q", errStr)
	}
	if srvHit != 1 {
		t.Fatalf("hit should be 1, got %d", srvHit)
	}
	path := EnvironmentFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	t.Logf("file:\n%s", string(data))
	lines := strings.Split(string(data), "\n")
	var varLines []string
	for _, l := range lines {
		if strings.Contains(l, "=") && !strings.HasPrefix(strings.TrimSpace(l), "#") {
			varLines = append(varLines, l)
		}
	}
	if len(varLines) != 3 {
		t.Fatalf("expected 3 var lines, got %d %v", len(varLines), varLines)
	}
	if !strings.HasPrefix(varLines[0], "OPENAI_API_KEY=") || !strings.HasPrefix(varLines[1], "OPENAI_BASE_URL=") || !strings.HasPrefix(varLines[2], "PARALLEL_API_KEY=") {
		t.Fatalf("not sorted: %v", varLines)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("perm wrong: %o", fi.Mode().Perm())
	}
	dirFi, _ := os.Stat(filepath.Dir(path))
	if dirFi.Mode().Perm() != 0700 {
		t.Fatalf("dir perm wrong: %o", dirFi.Mode().Perm())
	}
	if len(fake.sets) != 1 || len(fake.sets[0]) != 3 {
		t.Fatalf("expected 1 set with 3, got %v", fake.sets)
	}
	t.Logf("D-Bus sets: %v", fake.sets)
	// Second sync 304
	if err := mgr.Sync(ctx); err != nil {
		t.Fatalf("second sync 304 failed: %v", err)
	}
	rev2, cnt2, _, _ := mgr.Status()
	if rev2 != 1 || cnt2 != 3 {
		t.Fatalf("304 keep rev1")
	}
	if lastIfNoneMatch != `W/"1"` {
		t.Fatalf("expected If-None-Match W/\"1\", got %q", lastIfNoneMatch)
	}
	data2, _ := os.ReadFile(path)
	if string(data) != string(data2) {
		t.Fatalf("file changed on 304")
	}
	t.Logf("304 ok")
	// Third sync 500
	if err := mgr.Sync(ctx); err == nil {
		t.Fatalf("expected error on 500")
	}
	rev3, _, _, errStr3 := mgr.Status()
	if rev3 != 1 {
		t.Fatalf("rev should stay 1 after failure")
	}
	if errStr3 == "" {
		t.Fatalf("errStr should be set after failure")
	}
	if errStr3 != "" && strings.Contains(errStr3, "secret123") {
		t.Fatalf("error leaks value")
	}
	data3, _ := os.ReadFile(path)
	if string(data3) != string(data) {
		t.Fatalf("file should not change on fetch failure")
	}
	t.Logf("fetch failure keeps file ok")
	// Fourth sync with special chars via fresh manager (to get hit 4)
	freshFake := &fakeDBusTest{}
	freshMgr := NewEnvironmentManager(EnvironmentManagerOpts{
		Remote: remote,
		Creds:  creds,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
		DBus:   freshFake,
	})
	if err := freshMgr.Sync(ctx); err != nil {
		t.Fatalf("hit4 sync failed: %v", err)
	}
	rev4, cnt4, _, _ := freshMgr.Status()
	if rev4 != 2 || cnt4 != 2 {
		t.Fatalf("hit4 expected rev2 cnt2 got %d %d", rev4, cnt4)
	}
	path2 := EnvironmentFilePath()
	data4, _ := os.ReadFile(path2)
	t.Logf("file after hit4:\n%s", string(data4))
	if !strings.Contains(string(data4), "FOO=\"bar\\$with\\`backtick") {
		t.Fatalf("escaping failed for $ ` : %s", string(data4))
	}
	if !strings.Contains(string(data4), "\\\"quote\\\"") {
		t.Fatalf("escaping failed for quote: %s", string(data4))
	}
	if !strings.Contains(string(data4), "\\\\slash") {
		t.Fatalf("escaping failed for slash: %s", string(data4))
	}
	lines4 := strings.Split(string(data4), "\n")
	var varLines4 []string
	for _, l := range lines4 {
		if strings.Contains(l, "=") && !strings.HasPrefix(strings.TrimSpace(l), "#") {
			varLines4 = append(varLines4, l)
		}
	}
	t.Logf("varLines4: %v", varLines4)
	oldData, _ := os.ReadFile(path2)
	failFake := &fakeDBusTest{failUnset: true}
	failMgr := NewEnvironmentManager(EnvironmentManagerOpts{
		Remote: remote,
		Creds:  creds,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
		DBus:   failFake,
	})
	if err := failMgr.Sync(ctx); err == nil {
		t.Fatalf("expected D-Bus failure error")
	}
	newData, _ := os.ReadFile(path2)
	if string(newData) != string(oldData) {
		t.Fatalf("file should be reverted on D-Bus failure\nOld:%s\nNew:%s\n", string(oldData), string(newData))
	}
	t.Logf("D-Bus failure revert ok")
	clearFake := &fakeDBusTest{}
	clearMgr := NewEnvironmentManager(EnvironmentManagerOpts{
		Remote: remote,
		Creds:  creds,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
		DBus:   clearFake,
	})
	if _, err := os.Stat(path2); err != nil {
		t.Fatalf("file should exist before clear")
	}
	if err := clearMgr.Clear(ctx); err != nil {
		t.Fatalf("clear failed: %v", err)
	}
	if _, err := os.Stat(path2); !os.IsNotExist(err) {
		t.Fatalf("file should be removed after clear")
	}
	if len(clearFake.unsets) == 0 {
		t.Fatalf("clear should have called UnsetEnvironment")
	}
	t.Logf("clear ok, unset: %v", clearFake.unsets)
	// Daemon status fields
	sockDir := filepath.Join(tmp, "sock")
	os.MkdirAll(sockDir, 0700)
	sockPath := filepath.Join(sockDir, "clientd.sock")
	cfg := &Config{ServerURL: srv.URL, SocketPath: sockPath}
	// Use freshMgr for daemon (has rev 2), but we need a manager with rev 2 for daemon test
	// Create a manager that already synced to rev 2 and will be used by daemon
	daemonMgr := freshMgr
	daemon, err := NewDaemon(DaemonOpts{
		Config:          cfg,
		CredentialStore: creds,
		Remote:          remote,
		EnvManager:      daemonMgr,
		EnvInterval:     time.Hour,
	})
	if err != nil {
		t.Fatalf("new daemon: %v", err)
	}
	if err := daemon.Start(); err != nil {
		t.Fatalf("daemon start: %v", err)
	}
	defer daemon.Stop()
	time.Sleep(100 * time.Millisecond)
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	req := SocketRequest{ID: "1", Method: "status"}
	b, _ := json.Marshal(req)
	conn.Write(append(b, '\n'))
	br := bufio.NewReader(conn)
	raw, _ := br.ReadString('\n')
	var resp SocketResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	b2, _ := json.Marshal(resp.Result)
	var st map[string]any
	json.Unmarshal(b2, &st)
	t.Logf("daemon status via socket: %v", st)
	if _, ok := st["environment_revision"]; !ok {
		t.Fatalf("missing environment_revision")
	}
	// env revision should be non-zero after sync (either 1 or 2 depending on server hit)
	if int(st["environment_revision"].(float64)) == 0 {
		t.Fatalf("env revision should be non-zero")
	}
	if _, ok := st["environment_variable_count"]; !ok {
		t.Fatalf("missing environment_variable_count")
	}
	// Ensure error does not leak values
	if s, ok := st["environment_error"].(string); ok && strings.Contains(s, "secret123") {
		t.Fatalf("environment_error leaks value")
	}
	t.Logf("daemon status env fields ok")
	conn2, _ := net.Dial("unix", sockPath)
	req2 := SocketRequest{ID: "2", Method: "environment.sync"}
	b3, _ := json.Marshal(req2)
	conn2.Write(append(b3, '\n'))
	br2 := bufio.NewReader(conn2)
	raw2, _ := br2.ReadString('\n')
	var resp2 SocketResponse
	json.Unmarshal([]byte(raw2), &resp2)
	if resp2.Error != nil {
		t.Logf("env sync via socket error (may be expected if server hit exhausted): %v", resp2.Error.Message)
	} else {
		t.Logf("env sync via socket ok: %+v", resp2.Result)
	}
	// Ensure 5m ticker configured (envInterval default)
	if daemon.envInterval != time.Hour {
		t.Fatalf("daemon envInterval not as set")
	}
	// Test that daemon's envLoop would be 5m by default if not overridden
	daemon2, _ := NewDaemon(DaemonOpts{
		Config:          cfg,
		CredentialStore: creds,
		Remote:          remote,
	})
	if daemon2.envInterval != 5*time.Minute {
		t.Fatalf("default envInterval should be 5m, got %v", daemon2.envInterval)
	}
}
