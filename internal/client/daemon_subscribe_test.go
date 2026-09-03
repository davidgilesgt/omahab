package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

type mockState struct {
	mu       sync.Mutex
	events   []domain.Event
	status   domain.Status
	streamCh chan domain.Event
}

func newMockServer(t *testing.T, state *mockState) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/companion/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(state.status)
	})
	mux.HandleFunc("/api/v1/companion/events", func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		evs := append([]domain.Event(nil), state.events...)
		state.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": evs})
	})
	mux.HandleFunc("/api/v1/companion/events/stream", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(200)
		flusher.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case ev := <-state.streamCh:
				data, _ := json.Marshal(ev)
				fmt.Fprintf(w, "id: %s\n", ev.ID)
				fmt.Fprintf(w, "event: %s\n", ev.Type)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			case <-time.After(15 * time.Second):
				fmt.Fprintf(w, ": heartbeat\n\n")
				flusher.Flush()
			}
		}
	})
	mux.HandleFunc("/api/v1/companion/projects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []domain.Project{}})
	})
	mux.HandleFunc("/api/v1/companion/environment", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `W/"rev-0-abc"`)
		_ = json.NewEncoder(w).Encode(map[string]string{})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	})
	return httptest.NewServer(mux)
}

func TestDaemonSubscribePushWithin1s(t *testing.T) {
	state := &mockState{
		status: domain.Status{
			InstanceID: "test-instance",
			Version:    "1.0.0",
			Health:     "ok",
		},
		events:   []domain.Event{},
		streamCh: make(chan domain.Event, 10),
	}
	ts := newMockServer(t, state)
	defer ts.Close()

	sockPath := filepath.Join(os.TempDir(), fmt.Sprintf("omahab-test-%d-%d.sock", os.Getpid(), time.Now().UnixNano()%1000000))
	_ = os.Remove(sockPath)
	t.Cleanup(func() { _ = os.Remove(sockPath) })
	cfg := &Config{
		ServerURL:        ts.URL,
		PinnedInstanceID: "",
		SocketPath:       sockPath,
	}
	creds := NewMemoryCredentialStore()
	if err := creds.Set(CredentialService, CredentialDeviceAccount, "oma_dev_testtoken1234567890"); err != nil {
		t.Fatalf("set cred: %v", err)
	}
	remote, err := NewRemoteClient(RemoteClientConfig{
		ServerURL:        ts.URL,
		PinnedInstanceID: "",
		CredentialStore:  creds,
		HTTPClient:       ts.Client(),
	})
	if err != nil {
		t.Fatalf("remote: %v", err)
	}

	daemon, err := NewDaemon(DaemonOpts{
		Config:          cfg,
		CredentialStore: creds,
		Remote:          remote,
		ProjectStore:    NewProjectStore(&NopGitRunner{}),
		SyncInterval:    10 * time.Minute,
		FetchInterval:   10 * time.Minute,
		EnvInterval:     10 * time.Minute,
		BackupInterval:  10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("new daemon: %v", err)
	}
	if err := daemon.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = daemon.Stop()
		_ = os.Remove(sockPath)
	}()
	time.Sleep(300 * time.Millisecond)

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	subReq := SocketRequest{ID: "test-sub-1", Method: "subscribe", Params: map[string]any{}}
	b, _ := json.Marshal(subReq)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		t.Fatalf("write sub: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	var ack SocketResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &ack); err != nil {
		t.Fatalf("ack unmarshal %q: %v", line, err)
	}
	if ack.Error != nil {
		t.Fatalf("ack error: %+v", ack.Error)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err = br.ReadString('\n')
	if err != nil {
		t.Fatalf("read initial status: %v", err)
	}
	var initial map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &initial); err != nil {
		t.Fatalf("initial unmarshal %q: %v", line, err)
	}
	if initial["event"] != "status" {
		t.Fatalf("initial event want status got %v", initial["event"])
	}
	start := time.Now()
	ev := domain.Event{
		ID:        domain.ID("evt-test-1"),
		Type:      "agent.awaiting_approval",
		Severity:  "info",
		Message:   "waiting",
		CreatedAt: time.Now(),
	}
	state.mu.Lock()
	state.events = append(state.events, ev)
	state.mu.Unlock()
	select {
	case state.streamCh <- ev:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("streamCh blocked")
	}

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	deadline := time.Now().Add(3 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for pushed status after event")
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		line, err = br.ReadString('\n')
		if err != nil {
			if os.IsTimeout(err) {
				t.Fatalf("read timeout: %v", err)
			}
			t.Fatalf("read pushed status: %v", err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var msg map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("msg unmarshal %q: %v", line, err)
		}
		var eventStr string
		if raw, ok := msg["event"]; ok {
			_ = json.Unmarshal(raw, &eventStr)
		}
		if eventStr != "status" {
			continue
		}
		var data DaemonStatus
		if raw, ok := msg["data"]; ok {
			if err := json.Unmarshal(raw, &data); err != nil {
				t.Fatalf("data unmarshal: %v", err)
			}
		}
		elapsed := time.Since(start)
		if data.WaitingAgents > 0 || data.UnreadCount > 0 || data.UnreadEvents > 0 {
			t.Logf("pushed status delivered in %v waitingAgents=%d unread=%d", elapsed, data.WaitingAgents, data.UnreadCount)
			if elapsed > 1500*time.Millisecond {
				t.Fatalf("push too slow: %v > 1.5s", elapsed)
			}
			break
		}
		if elapsed > 2500*time.Millisecond {
			t.Fatalf("received status but not updated after 2.5s: %+v elapsed %v", data, elapsed)
		}
	}
	ev2 := domain.Event{
		ID:        domain.ID("evt-test-2"),
		Type:      "environment.changed",
		Severity:  "info",
		Message:   "env changed",
		CreatedAt: time.Now(),
	}
	state.mu.Lock()
	state.events = append(state.events, ev2)
	state.mu.Unlock()
	start2 := time.Now()
	state.streamCh <- ev2
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		line, err = br.ReadString('\n')
		if err != nil {
			t.Fatalf("read env pushed: %v", err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var msg map[string]json.RawMessage
		_ = json.Unmarshal([]byte(line), &msg)
		var es string
		if raw, ok := msg["event"]; ok {
			_ = json.Unmarshal(raw, &es)
		}
		if es != "status" {
			continue
		}
		elapsed := time.Since(start2)
		t.Logf("environment.changed pushed in %v", elapsed)
		if elapsed > 1500*time.Millisecond {
			t.Fatalf("env push too slow %v", elapsed)
		}
		break
	}
	_ = context.Background()
}
