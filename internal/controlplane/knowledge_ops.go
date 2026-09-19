package controlplane

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/knowledge"
	"github.com/omahab/omahab/internal/store"
)

func (b *Backend) KnowledgeSearch(ctx context.Context, principal, query string, limit int) ([]knowledge.Citation, error) {
	if b.knowledge == nil {
		return nil, translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	opts := knowledge.SearchOptions{Limit: limit}
	cits, err := b.knowledge.Search(ctx, principal, query, opts)
	if err != nil {
		return nil, translateError(err)
	}
	return cits, nil
}

func (b *Backend) KnowledgeGetMetadata(ctx context.Context, principal, docID string) (*knowledge.PaperlessMetadata, error) {
	if b.knowledge == nil {
		return nil, translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	m, err := b.knowledge.PaperlessGetMetadata(ctx, principal, docID)
	if err != nil {
		return nil, translateError(err)
	}
	return m, nil
}

func (b *Backend) KnowledgeGetText(ctx context.Context, principal, docID string) (string, error) {
	if b.knowledge == nil {
		return "", translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	txt, err := b.knowledge.PaperlessGetText(ctx, principal, docID)
	if err != nil {
		return "", translateError(err)
	}
	return txt, nil
}

func (b *Backend) KnowledgeListCorrespondents(ctx context.Context, principal string) ([]string, error) {
	if b.knowledge == nil {
		return nil, translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	list, err := b.knowledge.PaperlessListCorrespondents(ctx, principal)
	if err != nil {
		return nil, translateError(err)
	}
	return list, nil
}

func (b *Backend) KnowledgeListDocumentTypes(ctx context.Context, principal string) ([]string, error) {
	if b.knowledge == nil {
		return nil, translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	list, err := b.knowledge.PaperlessListDocumentTypes(ctx, principal)
	if err != nil {
		return nil, translateError(err)
	}
	return list, nil
}

func (b *Backend) KnowledgeListTags(ctx context.Context, principal string) ([]string, error) {
	if b.knowledge == nil {
		return nil, translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	list, err := b.knowledge.PaperlessListTags(ctx, principal)
	if err != nil {
		return nil, translateError(err)
	}
	return list, nil
}

func (b *Backend) KnowledgeUpload(ctx context.Context, principal, filename string, content []byte, tags []string) (string, error) {
	if b.knowledge == nil {
		return "", translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	id, err := b.knowledge.PaperlessUpload(ctx, principal, filename, content, tags)
	if err != nil {
		return "", translateError(err)
	}
	return id, nil
}

func (b *Backend) KnowledgeAddTag(ctx context.Context, principal, docID, tag string) error {
	if b.knowledge == nil {
		return translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	if err := b.knowledge.PaperlessAddTag(ctx, principal, docID, tag); err != nil {
		return translateError(err)
	}
	return nil
}

func (b *Backend) KnowledgeListSources(ctx context.Context) ([]*knowledge.Source, error) {
	if b.knowledge == nil {
		return nil, translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	list, err := b.knowledge.ListSources(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	return list, nil
}

func (b *Backend) KnowledgeIndexSetupOptions(ctx context.Context) ([]knowledge.IndexSetupOption, error) {
	return knowledge.IndexSetupOptions(), nil
}

func (b *Backend) KnowledgePinnedModels(ctx context.Context) ([]knowledge.ModelInfo, error) {
	if b.knowledge == nil {
		return nil, translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	pinned, err := knowledge.PinnedModels()
	if err != nil {
		// Display metadata only: a missing/unreadable backend file must
		// not 500 /api/v1/knowledge/pinned-models. Worst case the UI
		// shows "metadata not available yet" per model alias.
		return []knowledge.ModelInfo{}, nil
	}
	if pinned == nil {
		return []knowledge.ModelInfo{}, nil
	}
	return pinned, nil
}

func (b *Backend) KnowledgeGetSummarizationConsent(ctx context.Context, principal, provider string) (bool, error) {
	if b.knowledge == nil {
		return false, translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	has, err := b.knowledge.HasSummarizationConsent(ctx, principal, provider)
	if err != nil {
		return false, translateError(err)
	}
	return has, nil
}

func (b *Backend) KnowledgeSetSummarizationConsent(ctx context.Context, principal, provider string, granted bool) error {
	if b.knowledge == nil {
		return translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	if _, err := b.knowledge.SetSummarizationConsent(ctx, principal, provider, granted); err != nil {
		return translateError(err)
	}
	return nil
}

func (b *Backend) KnowledgeGetIndexSetup(ctx context.Context) (string, error) {
	if b.knowledge == nil {
		return "", translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	choice, err := b.knowledge.GetIndexSetupChoice(ctx)
	if err != nil {
		return "", translateError(err)
	}
	return choice, nil
}

func (b *Backend) KnowledgeSetIndexSetup(ctx context.Context, choice string) error {
	if b.knowledge == nil {
		return translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	if err := b.knowledge.SetIndexSetupChoice(ctx, choice); err != nil {
		return translateError(err)
	}
	return nil
}
func (b *Backend) GetHermesMCPToken(ctx context.Context) (string, error) {
	if b.secrets == nil {
		return "", translateError(fmt.Errorf("%w: secrets not configured", ErrNotConfigured))
	}
	v, err := b.secrets.RevealByName(ctx, "platform-app", "hermes_mcp_token")
	if err != nil {
		return "", translateError(err)
	}
	return v, nil
}

// ValidateHermesMCPToken checks the dedicated Hermes MCP token against the secret store.
// Missing secrets or empty tokens return false; secret-store failures return an error.
func (b *Backend) ValidateHermesMCPToken(ctx context.Context, token string) (bool, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return false, nil
	}
	if b.secrets == nil {
		return false, nil
	}
	stored, err := b.secrets.RevealByName(ctx, "platform-app", "hermes_mcp_token")
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	stored = strings.TrimSpace(stored)
	if stored == "" {
		return false, nil
	}
	got := sha256.Sum256([]byte(token))
	want := sha256.Sum256([]byte(stored))
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		return false, nil
	}
	return true, nil
}

func (b *Backend) RotateHermesMCPToken(ctx context.Context) (string, error) {
	if b.secrets == nil {
		return "", translateError(fmt.Errorf("%w: secrets not configured", ErrNotConfigured))
	}
	b.hermesTokenMu.Lock()
	defer b.hermesTokenMu.Unlock()
	newToken := generateRandomBase64URL(32)
	if newToken == "" {
		return "", translateError(fmt.Errorf("failed to generate token"))
	}
	if _, err := b.secrets.RotateByName(ctx, "platform-app", "hermes_mcp_token", newToken); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return "", translateError(err)
		}
		if _, perr := b.secrets.Put(ctx, "platform-app", "hermes_mcp_token", newToken); perr != nil {
			return "", translateError(perr)
		}
	}
	if err := b.refreshHermesMCPConsumer(ctx, newToken); err != nil {
		return newToken, translateError(fmt.Errorf("rotation completed but Hermes refresh failed: %w", err))
	}
	return newToken, nil
}

// refreshHermesMCPConsumer replaces only OMAHAB_MCP_TOKEN in the existing hermes
// environment and restarts a running unpaused Hermes bundle. A missing hermes
// environment file means Hermes is not configured, so refresh is skipped.
// The new secret is always retained; refresh failures never restore a revoked token.
func (b *Backend) refreshHermesMCPConsumer(ctx context.Context, newToken string) error {
	envPath := filepath.Join(b.appEnvDir(), "hermes.env")
	if _, err := os.Stat(envPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat hermes env: %w", err)
	}
	env, err := b.readAppEnv("hermes")
	if err != nil {
		return fmt.Errorf("read hermes env: %w", err)
	}
	env["OMAHAB_MCP_TOKEN"] = newToken
	if err := b.writeAppEnv("hermes", env, ""); err != nil {
		return fmt.Errorf("write hermes env: %w", err)
	}
	if b.apps == nil {
		return nil
	}
	if m, merr := apps.GetMaintenance(ctx, b.db, "hermes"); merr == nil {
		_ = m
		return nil
	}
	rows, err := b.appsByBundle(ctx, "hermes")
	if err != nil {
		return fmt.Errorf("list hermes apps: %w", err)
	}
	running := false
	for _, st := range rows {
		if st.ObservedState == apps.ObservedRunning {
			running = true
			break
		}
	}
	if !running {
		return nil
	}
	if err := b.redeployBundle(ctx, "hermes"); err != nil {
		return fmt.Errorf("restart hermes: %w", err)
	}
	return nil
}
// Identity extended
