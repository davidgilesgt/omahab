package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/providers"
	"github.com/omahab/omahab/internal/secrets"
	"github.com/omahab/omahab/internal/store"
	"gopkg.in/yaml.v3"
)

func (b *Backend) ensureHermesLiteLLMKey(ctx context.Context) error {
	if b.providers == nil || b.secrets == nil {
		log.Printf("setup dependent_apps: providers/secrets not configured; skipping hermes key")
		return nil
	}
	secretName := "hermes_litellm_key"
	var vkID string
	var expiresAt, revokedAt sql.NullString
	nowStr := store.FormatTime(time.Now().UTC())
	err := b.db.QueryRowContext(ctx, `SELECT id, expires_at, revoked_at FROM provider_virtual_keys WHERE owner_kind = 'hermes' AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?) ORDER BY created_at DESC LIMIT 1`, nowStr).Scan(&vkID, &expiresAt, &revokedAt)
	if err == nil {
		if v, rerr := b.secrets.RevealByName(ctx, "platform-app", secretName); rerr == nil && strings.TrimSpace(v) != "" {
			tok := strings.TrimSpace(v)
			if err := b.renderHermesKeyEnv(ctx, tok); err != nil {
				return err
			}
			if inst, err := b.store.Instance(ctx); err == nil {
				if d := strings.TrimSpace(inst.Domain); d != "" && d != "example.com" && d != "not-configured.invalid" {
					if err := b.renderHermesConfig(ctx, d); err != nil {
						return err
					}
				}
			}
			return nil
		}
		_ = b.providers.RevokeVirtualKey(ctx, domain.ID(vkID))
	}
	kind := providers.OwnerKindHermes
	ownerID := "hermes"
	res, err := b.providers.IssueVirtualKey(ctx, providers.IssueVirtualKeyInput{
		Name:      "hermes",
		Scopes:    []string{"omahab/fast", "omahab/balanced", "omahab/reasoning"},
		OwnerKind: &kind,
		OwnerID:   &ownerID,
	})
	if err != nil {
		return fmt.Errorf("issue hermes virtual key: %w", err)
	}
	if _, err := b.secrets.Put(ctx, "platform-app", secretName, res.Token); err != nil {
		if errors.Is(err, store.ErrConflict) || errors.Is(err, secrets.ErrConflict) {
			_, _ = b.secrets.RotateByName(ctx, "platform-app", secretName, res.Token)
		}
	}
	if err := b.renderHermesKeyEnv(ctx, res.Token); err != nil {
		return err
	}
	if inst, err := b.store.Instance(ctx); err == nil {
		if d := strings.TrimSpace(inst.Domain); d != "" && d != "example.com" && d != "not-configured.invalid" {
			if err := b.renderHermesConfig(ctx, d); err != nil {
				return err
			}
		}
	}
	return nil
}

// renderHermesKeyEnv writes the hermes appenv with the exact keys required by the stock image.
// It never writes HERMES_MODEL_GATEWAY_URL/KEY, HERMES_OIDC_*, or HERMES_PUBLIC_URL.

func (b *Backend) renderHermesKeyEnv(ctx context.Context, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("hermes token is required")
	}
	inst, _ := b.store.Instance(ctx)
	domainName := strings.TrimSpace(inst.Domain)
	var apiServerKey string
	var mcpToken string
	var oidcClientID string
	var hindsightAPIKey string
	if b.secrets != nil {
		if v, err := b.secrets.RevealByName(ctx, "platform-app", "hermes_api_server_key"); err == nil {
			apiServerKey = strings.TrimSpace(v)
		}
		if apiServerKey == "" {
			apiServerKey = generateRandomBase64URL(32)
			_ = upsertSecret(ctx, b.secrets, "platform-app", "hermes_api_server_key", apiServerKey)
		}
		if v, err := b.secrets.RevealByName(ctx, "platform-app", "hermes_mcp_token"); err == nil {
			mcpToken = strings.TrimSpace(v)
		}
		if mcpToken == "" {
			mcpToken = generateRandomBase64URL(32)
			_ = upsertSecret(ctx, b.secrets, "platform-app", "hermes_mcp_token", mcpToken)
		}
		if v, err := b.secrets.RevealByName(ctx, "platform-app", "hermes_oidc_client_id"); err == nil {
			oidcClientID = strings.TrimSpace(v)
		}
		if v, err := b.secrets.RevealByName(ctx, "platform-app", "hindsight_api_key"); err == nil {
			hindsightAPIKey = strings.TrimSpace(v)
		}
	}
	if hindsightAPIKey == "" {
		return fmt.Errorf("hindsight_api_key missing; run Hindsight setup before Hermes memory activation")
	}
	kv := map[string]string{
		"HERMES_UID":            "10000",
		"HERMES_GID":            "10000",
		"HERMES_DASHBOARD":      "1",
		// Host network mode (nix/apps.nix): bind loopback only, on the
		// same host ports the bridge publish used to remap (dashboard
		// 8085 = Caddy/health-check upstream, API 8642), so upstreams
		// need no changes and nothing is exposed beyond the host.
		"HERMES_DASHBOARD_HOST": "127.0.0.1",
		"HERMES_DASHBOARD_PORT": "8085",
		"API_SERVER_ENABLED":    "true",
		"API_SERVER_HOST":       "127.0.0.1",
		"API_SERVER_PORT":       "8642",
		"API_SERVER_KEY":        apiServerKey,
		// Host netns: host.docker.internal is gone; loopback is us.
		"OPENAI_BASE_URL":       "http://127.0.0.1:4000/v1",
		"OPENAI_API_KEY":        token,
		"ANTHROPIC_BASE_URL":    "http://127.0.0.1:4000",
		"ANTHROPIC_API_KEY":     token,
		"OMAHAB_MCP_TOKEN":      mcpToken,
		"HINDSIGHT_API_KEY":     hindsightAPIKey,
	}
	if domainName != "" && domainName != "example.com" && domainName != "not-configured.invalid" {
		kv["HERMES_DASHBOARD_PUBLIC_URL"] = "https://ai." + domainName
		kv["HERMES_DASHBOARD_OIDC_ISSUER"] = "https://id." + domainName
		if oidcClientID != "" {
			kv["HERMES_DASHBOARD_OIDC_CLIENT_ID"] = oidcClientID
		}
	} else if oidcClientID != "" {
		kv["HERMES_DASHBOARD_OIDC_CLIENT_ID"] = oidcClientID
	}
	prev, _ := b.readAppEnv("hermes")
	if err := b.writeAppEnv("hermes", kv, ""); err != nil {
		return fmt.Errorf("write hermes appenv: %w", err)
	}
	if maps.Equal(prev, kv) {
		log.Printf("setup dependent_apps: hermes env unchanged")
		return nil
	}
	log.Printf("setup dependent_apps: hermes env rendered")
	// Restart so a new LiteLLM token is picked up (mirrors renderHermesConfig).
	ctx2, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx2, "systemctl", "restart", "docker-hermes.service").Run(); err != nil {
		log.Printf("setup dependent_apps: hermes restart failed: %v", err)
	}
	return nil
}

const omahabServerSkillBody = `# Omahab Server MCP Tools

This skill describes the Omahab MCP server at `+"`http://127.0.0.1:8484/mcp`"+` (auth: `+"`Authorization: Bearer ${OMAHAB_MCP_TOKEN}`"+`). All tools return JSON text content.

## Repository tools

- `+"`repos_list()`"+` — list Forgejo repos you can access
- `+"`repo_get(owner, name)`"+` — get repo metadata and branch
- `+"`repo_archive(owner, name)`"+` / `+"`repo_unarchive(owner, name)`"+` — archive is reversible; never delete
- `+"`branches_list(owner, name)`"+` — list branches
- `+"`branch_create(owner, name, new_branch, from_ref)`"+` — create via Forgejo `+"`POST /repos/{o}/{r}/branches`"+`
- `+"`file_get(owner, name, path, ref)`"+` — read files
- `+"`issues_list/issue_get/issue_create/issue_comment`"+` — issue lifecycle
- `+"`prs_list/pr_get/pr_diff/pr_create/pr_comment`"+` — pull requests

Rules: archive, never delete; never force-push, never delete a branch, never merge a PR. `+"`repo_delete`"+`, branch delete, and PR merge tools do not exist and must not be emulated.

## Document tools

- `+"`docs_search(query, limit)`"+` / `+"`doc_get(id)`"+` — search Paperless and retrieve text
- `+"`docs_tags/docs_correspondents/docs_types`"+` — list facets
- `+"`doc_add_tag(id, tag)`"+` / `+"`doc_upload(filename, base64, tags)`"+` — tag and upload

Never delete documents; no delete tool exists.

## Notes (Obsidian vault via notesmd-cli)

The vault is bind-mounted read/write at `+"`/vault`"+`; the CLI is at `+"`/opt/notesmd/notesmd-cli`"+`. Prefer the CLI over raw file writes. One-time setup: `+"`notesmd-cli add-vault /vault --set-default`"+` (re-run it whenever `+"`list-vaults`"+` comes back empty; registration lives in the container config dir).
- `+"`search-content <query>`"+` / `+"`search <name>`"+` / `+"`print <note>`"+` / `+"`list`"+` — find and read
- `+"`create <note>`"+` / `+"`daily`"+` / `+"`move <src> <dst>`"+` / `+"`frontmatter <note>`"+` — write and organize
- Never touch `+"`/vault/.obsidian/`"+`, `+"`/vault/.trash/`"+`, or `+"`.stversions/`"+`. Never delete notes; archive by moving into an `+"`archive/`"+` folder.

## Project, CI, workspace, and status tools

- `+"`projects_list/project_get(slug)`"+` / `+"`releases_list(slug)`"+` / `+"`ci_runs(slug, limit)`"+` / `+"`ci_run_logs(slug, number)`"+` — project and pipeline state
- `+"`workspaces_list()`"+` / `+"`workspace_create(project_slug, task_title, instructions)`"+` / `+"`workspace_get(id)`"+` / `+"`workspace_send(id, message)`"+` / `+"`workspace_stop(id)`"+` — devcontainer workspaces
- `+"`events_recent(limit)`"+` / `+"`backup_status()`"+` — read-only control-plane status

## Branch / workspace conventions

Workspace creation always creates a branch `+"`ws/<slug>-<shortID>`"+` where `+"`slug`"+` is the slugified task title (lowercase, [a-z0-9-], ≤40) plus 4 hex chars, and workspace name is the branch with `+"`/`"+` → `+"`-`"+`. Example: title "add readme badge" → branch `+"`ws/add-readme-badge-a1b2`"+`. `+"`workspace_send`"+` injects into the tmux `+"`omp`"+` session. Never delete a workspace; use stop.

Always prefer archive and stop over delete.
`

func (b *Backend) renderHermesConfig(ctx context.Context, domainName string) error {
	domainName = strings.TrimSpace(domainName)
	if domainName == "" || domainName == "example.com" || domainName == "not-configured.invalid" {
		return nil
	}
	var oidcClientID string
	if b.secrets != nil {
		if v, err := b.secrets.RevealByName(ctx, "platform-app", "hermes_oidc_client_id"); err == nil {
			oidcClientID = strings.TrimSpace(v)
		}
	}
	stateDir := strings.TrimSpace(b.cfg.StateDir)
	if stateDir == "" {
		stateDir = "/var/lib/omahab"
	}
	hermesDir := filepath.Join(stateDir, "hermes")
	if err := os.MkdirAll(hermesDir, 0o700); err != nil {
		return fmt.Errorf("mkdir hermes: %w", err)
	}
	configPath := filepath.Join(hermesDir, "config.yaml")
	// Validate existing YAML and Hindsight JSON before replacing either file.
	if data, err := os.ReadFile(configPath); err == nil && len(data) > 0 {
		var check map[string]any
		if err := yaml.Unmarshal(data, &check); err != nil {
			return fmt.Errorf("invalid existing hermes config: %w", err)
		}
	}
	hindsightDir := filepath.Join(hermesDir, "hindsight")
	hindsightPath := filepath.Join(hindsightDir, "config.json")
	if data, err := os.ReadFile(hindsightPath); err == nil && len(data) > 0 {
		var check map[string]any
		if err := json.Unmarshal(data, &check); err != nil {
			return fmt.Errorf("invalid existing hindsight config: %w", err)
		}
	}
	if err := b.renderHindsightProviderJSON(hermesDir); err != nil {
		return err
	}
	desired := map[string]any{
		"model": map[string]any{
			"provider": "custom",
			"default":  "omahab/balanced",
			"base_url": "http://127.0.0.1:4000/v1",
		},
		"dashboard": map[string]any{
			"public_url":     "https://ai." + domainName,
			"trusted_proxies": []string{"127.0.0.1"},
			"oauth": map[string]any{
				"provider": "self-hosted",
				"self_hosted": map[string]any{
					"issuer":    "https://id." + domainName,
					"client_id": oidcClientID,
				},
			},
		},
		"approvals": map[string]any{
			"mode": "smart",
			"deny": []string{"rm -rf /*", "git push --force*", "git push -f*", "*--no-verify*"},
		},
		"mcp_servers": map[string]any{
			"omahab": map[string]any{
				"url": "http://127.0.0.1:8484/mcp",
				"headers": map[string]any{
					"Authorization": "Bearer ${OMAHAB_MCP_TOKEN}",
				},
			},
		},
		"memory": map[string]any{
			"provider": "hindsight",
		},
	}
	merged := desired
	if data, err := os.ReadFile(configPath); err == nil && len(data) > 0 {
		var existing map[string]any
		if err := yaml.Unmarshal(data, &existing); err == nil && existing != nil {
			for k, v := range desired {
				if ev, ok := existing[k]; ok {
					if em, ok := ev.(map[string]any); ok {
						if dm, ok := v.(map[string]any); ok {
							for dk, dv := range dm {
								if edv, ok := em[dk]; ok {
									if emNested, ok := edv.(map[string]any); ok {
										if dvNested, ok := dv.(map[string]any); ok {
											for ndk, ndv := range dvNested {
												emNested[ndk] = ndv
											}
											em[dk] = emNested
											continue
										}
									}
								}
								em[dk] = dv
							}
							existing[k] = em
							continue
						}
					}
				}
				existing[k] = v
			}
			merged = existing
		}
	}
	out, err := yaml.Marshal(merged)
	if err != nil {
		return fmt.Errorf("marshal hermes config: %w", err)
	}
	tmp := configPath + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("write hermes config tmp: %w", err)
	}
	_ = os.Chown(tmp, 10000, 10000)
	if err := os.Rename(tmp, configPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace hermes config: %w", err)
	}
	_ = os.Chmod(configPath, 0o600)
	skillsDir := filepath.Join(hermesDir, "skills", "omahab-server")
	if err := os.MkdirAll(skillsDir, 0o755); err == nil {
		skillPath := filepath.Join(skillsDir, "SKILL.md")
		skillContent := "---\nname: omahab-server\ndescription: Omahab server MCP tools and conventions\nversion: 1.0.0\n---\n" + omahabServerSkillBody
		tmp2 := skillPath + ".tmp"
		_ = os.WriteFile(tmp2, []byte(skillContent), 0o644)
		_ = os.Rename(tmp2, skillPath)
	}
	ctx2, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx2, "systemctl", "restart", "docker-hermes.service")
	_ = cmd.Run()
	return nil
}

func hindsightManagedJSON() map[string]any {
	return map[string]any{
		"mode":                 "local_external",
		"api_url":              "http://127.0.0.1:8888",
		"bank_id":              "hermes",
		"bank_id_template":     "",
		"memory_mode":          "hybrid",
		"recall_prefetch_method": "recall",
		"auto_recall":          true,
		"recall_sync":          true,
		"recall_budget":        "low",
		"recall_max_tokens":    1024,
		"recall_types":         []any{"world", "experience", "observation"},
		"auto_retain":          true,
		"retain_every_n_turns":  1,
		"retain_async":         true,
	}
}

func (b *Backend) renderHindsightProviderJSON(hermesDir string) error {
	_ = b
	dir := filepath.Join(hermesDir, "hindsight")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir hindsight: %w", err)
	}
	_ = os.Chown(dir, 10000, 10000)
	_ = os.Chmod(dir, 0o700)
	path := filepath.Join(dir, "config.json")
	merged := hindsightManagedJSON()
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		var existing map[string]any
		if err := json.Unmarshal(data, &existing); err != nil {
			return fmt.Errorf("invalid existing hindsight config: %w", err)
		}
		if existing != nil {
			delete(existing, "api_key")
			delete(existing, "apiKey")
			for k, v := range merged {
				existing[k] = v
			}
			merged = existing
		}
	} else {
		// No existing file: ensure managed map has no token fields.
		delete(merged, "api_key")
		delete(merged, "apiKey")
	}
	// Ensure token never lands in JSON even if a caller merged it.
	delete(merged, "api_key")
	delete(merged, "apiKey")
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal hindsight config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return fmt.Errorf("write hindsight config tmp: %w", err)
	}
	_ = os.Chown(tmp, 10000, 10000)
	_ = os.Chmod(tmp, 0o600)
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace hindsight config: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	return nil
}

func isDockerNotAvailable(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "executable file not found") || strings.Contains(s, "no such file or directory") || strings.Contains(s, "command not found") || strings.Contains(s, "not found")
}
