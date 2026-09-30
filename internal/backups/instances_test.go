package backups

import (
	"context"
	"testing"
)

// fakeDirLister enumerates a fixed set of directory names.
type fakeDirLister struct{ dirs []string }

func (f fakeDirLister) ListInstanceDirs(context.Context, string) ([]string, error) {
	return f.dirs, nil
}

// fakeProbeRunner accepts every repository and reports one snapshot.
type fakeProbeRunner struct{ probed []string }

func (f *fakeProbeRunner) Init(context.Context, Repository, Credentials) error { return nil }
func (f *fakeProbeRunner) Snapshots(context.Context, Repository, Credentials, int) ([]SnapshotListEntry, error) {
	return []SnapshotListEntry{{ID: "s1", Time: "2026-09-01T00:00:00Z", Hostname: "h"}}, nil
}
func (f *fakeProbeRunner) Forget(context.Context, Repository, Credentials, ForgetRequest) (ForgetResult, error) {
	return ForgetResult{}, nil
}
func (f *fakeProbeRunner) Stats(context.Context, Repository, Credentials) (RepoStats, error) {
	return RepoStats{}, nil
}
func (f *fakeProbeRunner) Check(context.Context, Repository, Credentials) error { return nil }
func (f *fakeProbeRunner) Backup(context.Context, Repository, Credentials, BackupRequest) (Snapshot, error) {
	return Snapshot{}, nil
}
func (f *fakeProbeRunner) Restore(context.Context, Repository, Credentials, string, string) error {
	return nil
}

// JoinInstanceLocation must accept only a single safe path component: a
// slash- or dotdot-bearing instance id can never escape the account's
// omahab child directory or select another repository.
func TestJoinInstanceLocationRejectsUnsafeIDs(t *testing.T) {
	root := "sftp://u@h.example:23/omahab"
	ok, err := JoinInstanceLocation(root, "omahab-2026")
	if err != nil || ok != "sftp://u@h.example:23/omahab/omahab-2026" {
		t.Fatalf("join = %q, %v", ok, err)
	}
	for _, id := range []string{
		"", ".", "..", "foo/bar", "foo/../other", "/etc", "./x", ".hidden",
		"foo\nbar", "foo bar", "a:b", "x\x00y", "ends-on-slash/",
	} {
		if got, err := JoinInstanceLocation(root, id); err == nil {
			t.Fatalf("JoinInstanceLocation(%q) = %q, want rejection", id, got)
		}
		if got, err := JoinInstanceLocation("/srv/backups", id); err == nil {
			t.Fatalf("local JoinInstanceLocation(%q) = %q, want rejection", id, got)
		}
	}
}

// ListInstances must skip enumerated ids that are not single safe path
// components instead of probing (or joining) them.
func TestListInstancesSkipsUnsafeIDs(t *testing.T) {
	lister := fakeDirLister{dirs: []string{"good-one", "foo/bar", "..escape", ".ssh", "ok.2"}}
	runner := &fakeProbeRunner{}
	out, err := ListInstances(context.Background(), lister, runner, "sftp://u@h.example:23/omahab", "pw")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range out {
		ids = append(ids, e.InstanceID)
		if e.Location != "sftp://u@h.example:23/omahab/"+e.InstanceID {
			t.Fatalf("location = %q for %q", e.Location, e.InstanceID)
		}
	}
	if len(ids) != 2 || ids[0] != "good-one" || ids[1] != "ok.2" {
		t.Fatalf("ids = %v, want [good-one ok.2]", ids)
	}
}
