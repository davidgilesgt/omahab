package syncer

import (
	"context"
	"strings"

	"github.com/omahab/omahab/internal/knowledge"
)

// KnowledgeService is the narrow knowledge API used by the syncer registrar.
// It mirrors knowledge.Service's source methods; SDK types do not leak past
// this boundary beyond domain Source.
type KnowledgeService interface {
	RegisterSource(ctx context.Context, kind, name, baseURL string) (*knowledge.Source, error)
	DeleteSource(ctx context.Context, id string) error
	ListSources(ctx context.Context) ([]*knowledge.Source, error)
}

type knowledgeBridge struct {
	svc KnowledgeService
}

// NewKnowledgeRegistrar returns a KnowledgeRegistrar that registers Syncthing
// folders with Share-with-AI enabled as knowledge sources of kind "notes".
// It scopes registration to the default Hermes profile; project bots never
// receive synced-folder sources (enforced by the caller).
func NewKnowledgeRegistrar(svc KnowledgeService) KnowledgeRegistrar {
	return &knowledgeBridge{svc: svc}
}

func (k *knowledgeBridge) Register(ctx context.Context, sourceID, serverPath string) error {
	name := strings.TrimSpace(sourceID)
	if name == "" {
		name = strings.TrimSpace(serverPath)
	}
	_, err := k.svc.RegisterSource(ctx, "notes", name, strings.TrimSpace(serverPath))
	if err != nil {
		if isKnowledgeConflict(err) {
			return nil
		}
		return err
	}
	return nil
}

func (k *knowledgeBridge) Unregister(ctx context.Context, sourceID string) error {
	id := strings.TrimSpace(sourceID)
	if id == "" {
		return nil
	}
	err := k.svc.DeleteSource(ctx, id)
	if err == nil {
		return nil
	}
	// Fallback: knowledge generated a different ID; find by name.
	sources, lerr := k.svc.ListSources(ctx)
	if lerr == nil {
		for _, src := range sources {
			if src.Kind == "notes" && src.Name == id {
				_ = k.svc.DeleteSource(ctx, src.ID)
				return nil
			}
			if src.BaseURL == id {
				_ = k.svc.DeleteSource(ctx, src.ID)
				return nil
			}
		}
	}
	if isKnowledgeNotFound(err) {
		return nil
	}
	return err
}

func isKnowledgeConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "conflict") || strings.Contains(msg, "already exists")
}

func isKnowledgeNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "no such") || strings.Contains(msg, "not_found")
}
