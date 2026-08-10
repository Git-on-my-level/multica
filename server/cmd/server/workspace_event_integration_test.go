package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestWorkspaceEventsAPIRequiresAuthAndFiltersWorkspace(t *testing.T) {
	if testPool == nil || testServer == nil {
		t.Skip("database unavailable")
	}
	ctx := context.Background()
	var baseline int64
	if err := testPool.QueryRow(ctx, `
		SELECT COALESCE((SELECT last_sequence FROM workspace_event_cursor WHERE workspace_id = $1), 0)
	`, testWorkspaceID).Scan(&baseline); err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	var ownAggregateID, foreignWorkspaceID, foreignAggregateID string
	if err := testPool.QueryRow(ctx, `SELECT gen_random_uuid()::text, gen_random_uuid()::text, gen_random_uuid()::text`).Scan(
		&ownAggregateID, &foreignWorkspaceID, &foreignAggregateID,
	); err != nil {
		t.Fatalf("generate ids: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace_event_outbox WHERE workspace_id IN ($1, $2)`, testWorkspaceID, foreignWorkspaceID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace_event_cursor WHERE workspace_id = $1`, foreignWorkspaceID)
	})
	if _, err := testPool.Exec(ctx, `
		SELECT append_workspace_event($1, $2, 'issue:updated', 'issue', $3, 'member', $4, '{}'::jsonb, now())
	`, testWorkspaceID, "api-own-"+ownAggregateID, ownAggregateID, testUserID); err != nil {
		t.Fatalf("append own event: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		SELECT append_workspace_event($1, $2, 'issue:updated', 'issue', $3, 'system', NULL, '{}'::jsonb, now())
	`, foreignWorkspaceID, "api-foreign-"+foreignAggregateID, foreignAggregateID); err != nil {
		t.Fatalf("append foreign event: %v", err)
	}

	resp := authRequest(t, http.MethodGet, fmt.Sprintf("/api/events?cursor=%d&limit=500", baseline), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var page struct {
		Events []struct {
			WorkspaceID string `json:"workspace_id"`
			AggregateID string `json:"aggregate_id"`
		} `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	foundOwn := false
	for _, event := range page.Events {
		if event.WorkspaceID != testWorkspaceID || event.AggregateID == foreignAggregateID {
			t.Fatalf("cross-workspace event leaked: %+v", event)
		}
		if event.AggregateID == ownAggregateID {
			foundOwn = true
		}
	}
	if !foundOwn {
		t.Fatalf("own event %s not returned: %+v", ownAggregateID, page.Events)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	unauthenticated, err := client.Get(testServer.URL + "/api/events?workspace_id=" + testWorkspaceID)
	if err != nil {
		t.Fatalf("unauthenticated request: %v", err)
	}
	defer unauthenticated.Body.Close()
	if unauthenticated.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", unauthenticated.StatusCode, http.StatusUnauthorized)
	}
}

func TestDeleteWorkspaceRemovesDurableEventAndClientKeyState(t *testing.T) {
	if testPool == nil || testServer == nil {
		t.Skip("database unavailable")
	}
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	var workspaceID, issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, description)
		VALUES ('Durable cleanup', $1, '') RETURNING id::text
	`, "durable-cleanup-"+suffix).Scan(&workspaceID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, workspaceID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue_create_idempotency WHERE workspace_id = $1`, workspaceID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace_event_outbox WHERE workspace_id = $1`, workspaceID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace_event_cursor WHERE workspace_id = $1`, workspaceID)
	})
	if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`, workspaceID, testUserID); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, creator_type, creator_id)
		VALUES ($1, 'cleanup issue', 'member', $2) RETURNING id::text
	`, workspaceID, testUserID).Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO issue_create_idempotency (workspace_id, client_key_hash, semantic_digest, issue_id)
		VALUES ($1, repeat('a', 64), repeat('b', 64), $2)
	`, workspaceID, issueID); err != nil {
		t.Fatalf("create client-key binding: %v", err)
	}

	req, err := http.NewRequest(http.MethodDelete, testServer.URL+"/api/workspaces/"+workspaceID, nil)
	if err != nil {
		t.Fatalf("build delete request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("X-Workspace-ID", workspaceID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete workspace: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}

	var eventRows, cursorRows, keyRows int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM workspace_event_outbox WHERE workspace_id = $1`, workspaceID).Scan(&eventRows); err != nil {
		t.Fatalf("count event rows: %v", err)
	}
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM workspace_event_cursor WHERE workspace_id = $1`, workspaceID).Scan(&cursorRows); err != nil {
		t.Fatalf("count cursor rows: %v", err)
	}
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM issue_create_idempotency WHERE workspace_id = $1`, workspaceID).Scan(&keyRows); err != nil {
		t.Fatalf("count client-key rows: %v", err)
	}
	if eventRows != 0 || cursorRows != 0 || keyRows != 0 {
		t.Fatalf("workspace teardown left event/cursor/client-key rows = %d/%d/%d", eventRows, cursorRows, keyRows)
	}
}
