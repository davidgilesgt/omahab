package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/omahab/omahab/internal/scm"
)

// resolveSCMWebhookSecret returns the HMAC secret for verifying Forgejo webhooks.
// It prefers the server's configured secret (from Config.SCMWebhookSecret or env),
// then falls back to the backend's ForgejoWebhookSecret method.
func (s *Server) resolveSCMWebhookSecret(r *http.Request) []byte {
	if len(s.scmWebhookSecret) != 0 {
		return s.scmWebhookSecret
	}
	if s.backend != nil {
		if sec, err := s.backend.ForgejoWebhookSecret(r.Context()); err == nil && strings.TrimSpace(sec) != "" {
			return []byte(strings.TrimSpace(sec))
		}
	}
	return nil
}

// handleSCMWebhook verifies X-Forgejo-Signature = hex HMAC-SHA256(raw body, secret)
// and dispatches pull_request (opened/synchronized/reopened) to Backend.OnPullRequest
// and push to Backend.OnPush. Other events return 204. Missing/invalid signature returns 401.
// On success returns 202 (accepted) so Forgejo stops retrying.
func (s *Server) handleSCMWebhook(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if isMaxBytesError(err, &maxErr) {
			writeError(w, r, errPayloadTooLarge("request body too large"))
			return
		}
		writeError(w, r, errBadRequest(err.Error()))
		return
	}
	// Need raw body for HMAC even if empty? Forgejo always sends JSON.
	if len(bytes.TrimSpace(raw)) == 0 {
		writeError(w, r, errBadRequest("empty request body"))
		return
	}

	secret := s.resolveSCMWebhookSecret(r)
	if len(secret) == 0 {
		writeError(w, r, errUnauthorized("webhook secret not configured"))
		return
	}

	sigHeader := strings.TrimSpace(r.Header.Get("X-Forgejo-Signature"))
	if strings.TrimSpace(sigHeader) == "" {
		// Try fallback header X-Gitea-Signature
		sigHeader = strings.TrimSpace(r.Header.Get("X-Gitea-Signature"))
	}
	if strings.TrimSpace(sigHeader) == "" {
		writeError(w, r, errUnauthorized("missing X-Forgejo-Signature"))
		return
	}
	if !scm.VerifyWebhookSignature(secret, raw, sigHeader) {
		writeError(w, r, errUnauthorized("invalid HMAC signature"))
		return
	}

	event := strings.TrimSpace(r.Header.Get("X-Forgejo-Event"))
	switch event {
	case "pull_request":
		// Parse pull_request payload
		var payload struct {
			Action      string `json:"action"`
			PullRequest *struct {
				Number  int64  `json:"number"`
				Index   int64  `json:"index"`
				Title   string `json:"title"`
				Body    string `json:"body"`
				State   string `json:"state"`
				HTMLURL string `json:"html_url"`
				User    *struct {
					Login string `json:"login"`
				} `json:"user"`
				Head *struct {
					SHA  string `json:"sha"`
					Ref  string `json:"ref"`
					Repo *struct {
						FullName string `json:"full_name"`
					} `json:"repo"`
				} `json:"head"`
				Base *struct {
					Ref  string `json:"ref"`
					Repo *struct {
						FullName string `json:"full_name"`
					} `json:"repo"`
				} `json:"base"`
			} `json:"pull_request"`
			Repository *struct {
				FullName string `json:"full_name"`
				Owner    *struct {
					Login string `json:"login"`
				} `json:"owner"`
				Name string `json:"name"`
			} `json:"repository"`
			Sender *struct {
				Login string `json:"login"`
			} `json:"sender"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			writeError(w, r, errBadRequest("invalid pull_request payload: "+err.Error()))
			return
		}
		act := strings.ToLower(strings.TrimSpace(payload.Action))
		if act != "opened" && act != "synchronized" && act != "reopened" {
			// Not in the set that triggers review; acknowledge.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if payload.PullRequest == nil || payload.Repository == nil {
			writeError(w, r, errBadRequest("pull_request payload missing fields"))
			return
		}
		// Resolve owner/name
		owner := ""
		name := ""
		full := strings.TrimSpace(payload.Repository.FullName)
		if full != "" {
			parts := strings.SplitN(full, "/", 2)
			if len(parts) == 2 {
				owner = parts[0]
				name = parts[1]
			}
		}
		if owner == "" && payload.Repository.Owner != nil {
			owner = payload.Repository.Owner.Login
		}
		if name == "" {
			name = payload.Repository.Name
		}
		if owner == "" || name == "" {
			writeError(w, r, errBadRequest("repository owner/name missing"))
			return
		}
		pr := payload.PullRequest
		pull := scm.PullRequest{
			Title:   pr.Title,
			Body:    pr.Body,
			State:   pr.State,
			HTMLURL: pr.HTMLURL,
		}
		if pr.Number != 0 {
			pull.Index = pr.Number
		} else {
			pull.Index = pr.Index
		}
		if pr.User != nil {
			pull.Author = pr.User.Login
		}
		if pr.Head != nil {
			pull.HeadSHA = pr.Head.SHA
			pull.HeadBranch = pr.Head.Ref
			if pr.Head.Repo != nil {
				pull.HeadRepoFullName = pr.Head.Repo.FullName
			}
		}
		if pr.Base != nil {
			pull.BaseBranch = pr.Base.Ref
			if pr.Base.Repo != nil {
				pull.BaseRepoFullName = pr.Base.Repo.FullName
			}
		}
		sender := ""
		if payload.Sender != nil {
			sender = payload.Sender.Login
		}
		ev := scm.PullRequestEvent{
			Action:     act,
			Repository: scm.RepoRef{Owner: owner, Name: name},
			PullRequest: pull,
			Sender:     sender,
		}
		if err := s.backend.OnPullRequest(r.Context(), ev); err != nil {
			writeError(w, r, err)
			return
		}
		// Accepted for processing
		w.WriteHeader(http.StatusAccepted)
		return
	case "push":
		var payload struct {
			Ref        string `json:"ref"`
			Before     string `json:"before"`
			After      string `json:"after"`
			Repository *struct {
				FullName string `json:"full_name"`
				Owner    *struct {
					Login string `json:"login"`
				} `json:"owner"`
				Name string `json:"name"`
			} `json:"repository"`
			Pusher *struct {
				Login string `json:"login"`
				Name  string `json:"name"`
			} `json:"pusher"`
			Sender *struct {
				Login string `json:"login"`
			} `json:"sender"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			writeError(w, r, errBadRequest("invalid push payload: "+err.Error()))
			return
		}
		if payload.Repository == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		owner := ""
		name := ""
		full := strings.TrimSpace(payload.Repository.FullName)
		if full != "" {
			parts := strings.SplitN(full, "/", 2)
			if len(parts) == 2 {
				owner = parts[0]
				name = parts[1]
			}
		}
		if owner == "" && payload.Repository.Owner != nil {
			owner = payload.Repository.Owner.Login
		}
		if name == "" {
			name = payload.Repository.Name
		}
		if owner == "" || name == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		sender := ""
		if payload.Pusher != nil && payload.Pusher.Login != "" {
			sender = payload.Pusher.Login
		} else if payload.Pusher != nil && payload.Pusher.Name != "" {
			sender = payload.Pusher.Name
		} else if payload.Sender != nil {
			sender = payload.Sender.Login
		}
		ev := scm.PushEvent{
			Repository: scm.RepoRef{Owner: owner, Name: name},
			Ref:        payload.Ref,
			BeforeSHA:  payload.Before,
			AfterSHA:   payload.After,
			Sender:     sender,
		}
		_ = s.backend.OnPush(r.Context(), ev)
		w.WriteHeader(http.StatusAccepted)
		return
	default:
		// Other events ignored per spec -> 204
		w.WriteHeader(http.StatusNoContent)
		return
	}
}

func isMaxBytesError(err error, target **http.MaxBytesError) bool {
	if err == nil || target == nil {
		return false
	}
	return errors.As(err, target)
}
