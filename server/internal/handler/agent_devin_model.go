package handler

import (
	"context"
	"fmt"
	"strings"

	"github.com/multica-ai/multica/server/pkg/agent"
)

// Validate only explicit routing changes. Unrelated edits must remain possible
// when a catalog drifts; session confirmation still guards every actual run.
func (h *Handler) validateDevinModelSelection(ctx context.Context, provider, runtimeID, requested string) error {
	requested = strings.TrimSpace(requested)
	if provider != "devin" || requested == "" {
		return nil
	}
	snapshot := h.cachedModelCatalog(ctx, runtimeID)
	if snapshot == nil {
		return fmt.Errorf("cannot validate requested Devin model %q: runtime model catalog is unavailable; open the runtime model picker to discover available IDs and retry", requested)
	}
	for _, model := range snapshot.Models {
		if agent.DevinModelIDMatches(requested, model.ID) {
			return nil
		}
	}
	return fmt.Errorf("requested Devin model %q is unavailable in this runtime's discovered catalog; refresh the runtime model picker and choose an available model ID", requested)
}
