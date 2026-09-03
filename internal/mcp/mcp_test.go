package mcp

import "testing"

func TestToolSurface(t *testing.T) {
	srv := New(Deps{})
	names := srv.ToolNames()
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	// Must be absent
	for _, forbidden := range []string{"repo_delete", "doc_delete", "pr_merge", "workspace_delete"} {
		if set[forbidden] {
			t.Fatalf("tool %q must be absent", forbidden)
		}
	}
	// Also ensure related destructive exclusions absent
	for _, forbidden := range []string{"repo_delete", "branch_delete", "force_push", "pr_merge", "workspace_delete", "doc_delete"} {
		if set[forbidden] {
			t.Fatalf("tool %q must be absent", forbidden)
		}
	}
	if !set["repo_archive"] {
		t.Fatalf("repo_archive must be present; got %v", names)
	}
	// Ensure expected tools present
	expected := []string{
		"repos_list", "repo_get", "repo_archive", "repo_unarchive",
		"branches_list", "branch_create", "file_get", "file_put",
		"issues_list", "issue_get", "issue_create", "issue_comment",
		"prs_list", "pr_get", "pr_diff", "pr_create", "pr_comment",
		"docs_search", "doc_get", "docs_tags", "docs_correspondents", "docs_types", "doc_add_tag", "doc_upload",
		"projects_list", "project_get", "releases_list", "ci_runs", "ci_run_logs",
		"workspaces_list", "workspace_create", "workspace_get", "workspace_send", "workspace_stop",
		"events_recent", "backup_status",
	}
	for _, n := range expected {
		if !set[n] {
			t.Fatalf("expected tool %q missing; got %v", n, names)
		}
	}
	if len(names) != len(expected) {
		t.Fatalf("unexpected tool count: want %d got %d (%v)", len(expected), len(names), names)
	}
}
