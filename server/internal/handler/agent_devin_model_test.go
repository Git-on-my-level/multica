package handler

import (
	"context"
	"strings"
	"testing"
)

func TestDevinModelSelectionConfiguration(t *testing.T) {
	const model = "fusion-gpt-6-1-sol-high-sidekick-swe-2-medium"
	ctx := context.Background()
	cache := NewInMemoryModelCatalogCache()
	if err := cache.Put(ctx, "m1", []ModelEntry{{ID: model}}, nil, true); err != nil {
		t.Fatal(err)
	}
	h := &Handler{ModelCatalogCache: cache}
	for _, tc := range []struct{ provider, runtime, requested, want string }{
		{"devin", "m1", model, ""},
		{"devin", "m1", "fusion-gpt-6.1-sol-high-sidekick-swe-2-medium", ""},
		{"devin", "m1", "fusion-gpt-6.1-sol-high-sidekick-swe-2-high", "unavailable"},
		{"devin", "other-runtime", model, "has not reported"},
		{"devin", "m1", "", ""},
		{"codex", "m1", "unrelated-model", ""},
	} {
		t.Run(tc.provider+"/"+tc.runtime+"/"+tc.requested, func(t *testing.T) {
			err := h.validateDevinModelSelection(ctx, tc.provider, tc.runtime, tc.requested)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.requested) || !strings.Contains(err.Error(), "model picker") {
				t.Fatalf("missing requested ID or recovery guidance: %v", err)
			}
		})
	}
}
