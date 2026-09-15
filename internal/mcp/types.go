package mcp

import (
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/knowledge"
)

// Document is the Paperless document view returned by doc_get: metadata plus
// extracted text. Text-fetch failure is an error, never partial success.
type Document struct {
	Metadata *knowledge.PaperlessMetadata `json:"metadata"`
	Text     string                       `json:"text"`
}

// Repository is the Forgejo repository view returned by repo_get.
type Repository struct {
	Owner          string `json:"owner"`
	Name           string `json:"name"`
	CloneURL       string `json:"clone_url"`
	Private        bool   `json:"private"`
	DefaultBranch  string `json:"default_branch"`
	ActionsEnabled bool   `json:"actions_enabled"`
	RemoteID       int64  `json:"remote_id"`
	Archived       bool   `json:"archived"`
}

// Issue is the Forgejo issue view returned by issue_get and issue_create.
type Issue struct {
	Owner   string `json:"owner"`
	Name    string `json:"name"`
	Index   int64  `json:"index"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	State   string `json:"state"`
	Author  string `json:"author"`
	HTMLURL string `json:"html_url"`
}

// PullRequest is the Forgejo pull request view returned by pr_get and
// pr_create. Head and base carry branch names.
type PullRequest struct {
	Owner   string `json:"owner"`
	Name    string `json:"name"`
	Index   int64  `json:"index"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	State   string `json:"state"`
	Author  string `json:"author"`
	HTMLURL string `json:"html_url"`
	Head    string `json:"head"`
	Base    string `json:"base"`
	HeadSHA string `json:"head_sha"`
}

// Transport projections preserve the existing restricted wire surface for
// map-shaped list results. Keys and underlying field types match the maps
// they replace; no new omission tags, no additional exposed fields.

// RepositorySummary is the managed-project listing entry from repos_list.
type RepositorySummary struct {
	Owner string    `json:"owner"`
	Name  string    `json:"name"`
	Slug  string    `json:"slug"`
	ID    domain.ID `json:"id"`
}

// ProjectSummary is the projects_list / project_get entry.
type ProjectSummary struct {
	ID            domain.ID `json:"id"`
	Slug          string    `json:"slug"`
	Name          string    `json:"name"`
	RepositoryURL string    `json:"repository_url"`
	Hostname      string    `json:"hostname"`
	CreatedAt     time.Time `json:"created_at"`
}

// ReleaseSummary is the releases_list entry.
type ReleaseSummary struct {
	ID        domain.ID `json:"id"`
	ProjectID domain.ID `json:"project_id"`
	Commit    string    `json:"commit"`
	Digest    string    `json:"digest"`
	Status    string    `json:"status"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
}

// RunSummary is the ci_runs entry.
type RunSummary struct {
	ID        domain.ID `json:"id"`
	Number    int       `json:"number"`
	Status    string    `json:"status"`
	CommitSHA string    `json:"commit_sha"`
	Branch    string    `json:"branch"`
	Event     string    `json:"event"`
	CreatedAt time.Time `json:"created_at"`
	Author    string    `json:"author"`
}

// EventSummary is the events_recent entry.
type EventSummary struct {
	ID        domain.ID      `json:"id"`
	Type      string         `json:"type"`
	Severity  string         `json:"severity"`
	Message   string         `json:"message"`
	Data      map[string]any `json:"data"`
	CreatedAt time.Time      `json:"created_at"`
}
