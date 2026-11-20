package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
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
		{"devin", "m1", "  " + model + "  ", ""},
		{"devin", "m1", "fusion-gpt-6.1-sol-high-sidekick-swe-2-medium", ""},
		{"devin", "m1", "fusion-gpt-6.1-sol-high-sidekick-swe-2-high", "unavailable"},
		{"devin", "other-runtime", model, "catalog is unavailable"},
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
	failedCache := &Handler{ModelCatalogCache: failingModelCatalogCache{}}
	if err := failedCache.validateDevinModelSelection(ctx, "devin", "m1", model); err == nil || !strings.Contains(err.Error(), "catalog is unavailable") {
		t.Fatalf("cache failure must fail closed with accurate guidance: %v", err)
	}
}

func TestDevinModelSelectionEndpoints(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	const model = "fusion-gpt-6-1-sol-high-sidekick-swe-2-medium"
	runtimeID := dbfx.Runtime(t, "Devin selection", testutil.Cols{"provider": "devin"})
	otherID := dbfx.Runtime(t, "Other Devin selection", testutil.Cols{"provider": "devin"})
	h := *testHandler
	h.ModelCatalogCache = NewInMemoryModelCatalogCache()
	if err := h.ModelCatalogCache.Put(ctx, runtimeID, []ModelEntry{{ID: model}}, nil, true); err != nil {
		t.Fatal(err)
	}
	for name, endpoint := range map[string]func(http.ResponseWriter, *http.Request){
		"builder": h.CreateAgentBuilderSession,
		"mika":    h.CreateMikaAgent,
	} {
		t.Run(name+" rejects unavailable model", func(t *testing.T) {
			w := httptest.NewRecorder()
			req := withChatTestWorkspaceCtx(t, newRequest(http.MethodPost, "/api/agents", map[string]any{"runtime_id": runtimeID, "model": "missing-model", "language": "en"}))
			endpoint(w, req)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "missing-model") {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
		})
	}

	for _, requested := range []string{"missing-model", model} {
		w := httptest.NewRecorder()
		h.CreateAgent(w, newRequest(http.MethodPost, "/api/agents", map[string]any{"name": "devin-selection-endpoint", "runtime_id": runtimeID, "model": requested}))
		if requested == "missing-model" {
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), requested) {
				t.Fatalf("unavailable create: %d %s", w.Code, w.Body.String())
			}
		} else {
			if w.Code != http.StatusCreated {
				t.Fatalf("available create: %d %s", w.Code, w.Body.String())
			}
			var created struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM agent WHERE id = $1", created.ID) })
		}
	}
	agentID := dbfx.Agent(t, "Devin selection update", runtimeID, testutil.Cols{"model": model})
	for _, tc := range []struct {
		name   string
		patch  map[string]any
		status int
	}{
		{"unavailable update", map[string]any{"model": "missing-model"}, http.StatusBadRequest},
		{"available update", map[string]any{"model": model}, http.StatusOK},
		{"target runtime catalog", map[string]any{"runtime_id": otherID}, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.UpdateAgent(w, withURLParam(newRequest(http.MethodPatch, "/api/agents/"+agentID, tc.patch), "id", agentID))
			if w.Code != tc.status {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
		})
	}
	if err := h.ModelCatalogCache.Put(ctx, runtimeID, []ModelEntry{{ID: "different-model"}}, nil, true); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.UpdateAgent(w, withURLParam(newRequest(http.MethodPatch, "/api/agents/"+agentID, map[string]any{"name": "Devin selection renamed"}), "id", agentID))
	if w.Code != http.StatusOK {
		t.Fatalf("unrelated edit after drift: %d %s", w.Code, w.Body.String())
	}
}
