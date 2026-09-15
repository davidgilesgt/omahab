package backups

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Attach verifies with snapshots and never initializes.
func TestAttachUsesSnapshotsNeverInit(t *testing.T) {
	svc, _, fr := newTestService(t, nil)
	repo, err := svc.AttachRepository(context.Background(), ConfigureRequest{
		Label:     "adopted",
		Location:  "sftp://u@host:23/./omahab/source-id",
		SecretRef: SecretRef{ID: "s", Version: 1},
	})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !repo.Attached {
		t.Fatal("attached flag not set")
	}
	if repo.Location != "sftp://u@host:23/./omahab/source-id" {
		t.Fatalf("location = %q, want exact source URL", repo.Location)
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if len(fr.initCalls) != 0 {
		t.Fatalf("init called %d times on attach; must never init", len(fr.initCalls))
	}
}

// Empty sources fail the attach and leave no phantom row.
func TestAttachEmptySourceFails(t *testing.T) {
	svc, _, fr := newTestService(t, func(_ *Config, deps *Deps) {
		fr := deps.Runner.(*fakeRunner)
		fr.snapshotsFn = func(Repository, Credentials, int) ([]SnapshotListEntry, error) {
			return nil, nil
		}
	})
	_ = fr
	_, err := svc.AttachRepository(context.Background(), ConfigureRequest{
		Label:     "empty",
		Location:  "sftp://u@host:23/./omahab/empty",
		SecretRef: SecretRef{ID: "s", Version: 1},
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid for empty source", err)
	}
	repos, err := svc.Repositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 0 {
		t.Fatalf("repos = %+v, want no phantom row", repos)
	}
}

// Unreadable sources fail the attach and leave no phantom row.
func TestAttachUnreadableSourceFails(t *testing.T) {
	svc, _, _ := newTestService(t, func(_ *Config, deps *Deps) {
		fr := deps.Runner.(*fakeRunner)
		fr.snapshotsFn = func(Repository, Credentials, int) ([]SnapshotListEntry, error) {
			return nil, errors.New("permission denied")
		}
	})
	_, err := svc.AttachRepository(context.Background(), ConfigureRequest{
		Label:     "locked",
		Location:  "sftp://u@host:23/./omahab/locked",
		SecretRef: SecretRef{ID: "s", Version: 1},
	})
	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("err = %v, want unreadable", err)
	}
	repos, err := svc.Repositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 0 {
		t.Fatalf("repos = %+v, want no phantom row", repos)
	}
}

// Attached repositories keep their exact source suffix; created ones gain the
// current instance suffix.
func TestScopedRepositoryAttachedExact(t *testing.T) {
	svc, _, _ := newTestService(t, func(_ *Config, deps *Deps) {
		deps.InstanceID = func(context.Context) string { return "new-machine" }
	})
	ctx := context.Background()
	attached := Repository{ID: "a", Location: "sftp://u@host:23/./omahab/source-id", Attached: true}
	if got := svc.scopedRepository(ctx, attached); got.Location != attached.Location {
		t.Fatalf("attached scoped = %q, want exact", got.Location)
	}
	created := Repository{ID: "b", Location: "sftp://u@host:23/./omahab"}
	if got := svc.scopedRepository(ctx, created); got.Location != "sftp://u@host:23/./omahab/new-machine" {
		t.Fatalf("created scoped = %q", got.Location)
	}
}

func TestSFTPCommandPortAndQuoting(t *testing.T) {
	hetzner := Repository{Location: "sftp://u123@u123.your-storagebox.de:23/./omahab/x"}
	args := sftpCommandArgs(hetzner)
	if len(args) != 2 || !strings.Contains(args[1], "-p '23'") {
		t.Fatalf("hetzner args = %v, want explicit port 23", args)
	}
	generic := Repository{Location: "sftp://backup@example.com/backups"}
	args = sftpCommandArgs(generic)
	if len(args) != 2 || !strings.Contains(args[1], "-p '22'") {
		t.Fatalf("generic args = %v, want default port 22", args)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "StrictHostKeyChecking=yes") {
		t.Fatalf("args = %v, want host-key enforcement", args)
	}
	if strings.Contains(joined, "example.com -s") {
		t.Fatalf("args = %v, want quoted user@host", args)
	}
	// No user: default ssh, no injection surface.
	if got := sftpCommandArgs(Repository{Location: "sftp://example.com/backups"}); got != nil {
		t.Fatalf("no-user args = %v, want nil", got)
	}
	// Host with shell metacharacters: refuse to inject.
	if got := sftpCommandArgs(Repository{Location: "sftp://u@evil.com;rm -rf/x"}); got != nil {
		t.Fatalf("metachar args = %v, want nil", got)
	}
}
