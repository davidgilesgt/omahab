package drops

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		want Target
	}{
		{"scan.pdf", TargetPaperless},
		{"SCAN.PDF", TargetPaperless},
		{"notes.docx", TargetPaperless},
		{"IMG_1234.heic", TargetPhotos},
		{"vid.MP4", TargetPhotos},
		{"photo.jpg", TargetPhotos},
		{"movie.mkv", TargetMedia},
		{"album.flac", TargetMedia},
		{"song.mp3", TargetMedia},
		{"archive.zip", TargetKeep},
		{"README", TargetKeep},
		{".stignore", TargetKeep},
		{".hidden.pdf", TargetKeep},
		{"", TargetKeep},
	}
	for _, tc := range cases {
		if got := Classify(tc.name); got != tc.want {
			t.Errorf("Classify(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestRouteInbox(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "inbox")
	dests := Destinations{
		Paperless: filepath.Join(root, "paperless"),
		Photos:    filepath.Join(root, "photos"),
		Media:     filepath.Join(root, "media"),
	}
	files := map[string]string{
		"a.pdf":   "pdf",
		"b.jpg":   "jpg",
		"c.mkv":   "mkv",
		"keep.me": "x",
		"noext":   "x",
	}
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(inbox, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Subdirectory and symlink must be left alone.
	if err := os.MkdirAll(filepath.Join(inbox, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.pdf", filepath.Join(inbox, "link.pdf")); err != nil {
		t.Fatal(err)
	}

	res, err := RouteInbox(inbox, dests)
	if err != nil {
		t.Fatalf("RouteInbox: %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("routed %d files, want 3: %+v", len(res), res)
	}
	for name, target := range map[string]Target{"a.pdf": TargetPaperless, "b.jpg": TargetPhotos, "c.mkv": TargetMedia} {
		found := false
		for _, r := range res {
			if r.File == name && r.Target == target {
				found = true
			}
		}
		if !found {
			t.Errorf("missing route for %s -> %s", name, target)
		}
	}
	// keep.me, noext and subdir stay. link.pdf stays as a (now dangling)
	// symlink — Lstat, since Stat follows the moved target.
	for _, name := range []string{"keep.me", "noext", "subdir"} {
		if _, err := os.Stat(filepath.Join(inbox, name)); err != nil {
			t.Errorf("inbox should still hold %s: %v", name, err)
		}
	}
	if fi, err := os.Lstat(filepath.Join(inbox, "link.pdf")); err != nil {
		t.Errorf("inbox should still hold link.pdf: %v", err)
	} else if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("link.pdf should still be a symlink (router must not follow it)")
	}
}
func TestRouteInboxCollision(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "inbox")
	dests := Destinations{
		Paperless: filepath.Join(root, "paperless"),
		Photos:    filepath.Join(root, "photos"),
		Media:     filepath.Join(root, "media"),
	}
	if err := os.MkdirAll(dests.Paperless, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dests.Paperless, "a.pdf"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inbox, "a.pdf"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := RouteInbox(inbox, dests)
	if err != nil {
		t.Fatalf("RouteInbox: %v", err)
	}
	if len(res) != 1 || filepath.Base(res[0].To) != "a-2.pdf" {
		t.Fatalf("collision should rename to a-2.pdf, got %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dests.Paperless, "a.pdf")); err != nil {
		t.Fatal("original must survive collision")
	}
}
