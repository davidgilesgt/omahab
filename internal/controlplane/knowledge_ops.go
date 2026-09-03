package controlplane

import (
	"context"
	"fmt"
	"github.com/omahab/omahab/internal/knowledge"
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
		return nil, translateError(err)
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

func (b *Backend) RotateHermesMCPToken(ctx context.Context) (string, error) {
	if b.secrets == nil {
		return "", translateError(fmt.Errorf("%w: secrets not configured", ErrNotConfigured))
	}
	newToken := generateRandomBase64URL(32)
	if newToken == "" {
		return "", translateError(fmt.Errorf("failed to generate token"))
	}
	if _, err := b.secrets.RotateByName(ctx, "platform-app", "hermes_mcp_token", newToken); err != nil {
		// If rotate fails because not found, try Put.
		if _, perr := b.secrets.Put(ctx, "platform-app", "hermes_mcp_token", newToken); perr != nil {
			return "", translateError(perr)
		}
	}
	return newToken, nil
}
// Identity extended
