package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type idempotentIssueResponse struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Code  string `json:"code"`
}

func testClientKey(seed string) string {
	digest := sha256.Sum256([]byte(seed))
	return fmt.Sprintf("sha256:%x", digest[:])
}

func postIssueForWorkspace(workspaceID string, body any) (*http.Response, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, testServer.URL+"/api/issues?workspace_id="+workspaceID, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("X-Workspace-ID", workspaceID)
	return http.DefaultClient.Do(req)
}

func decodeIdempotentIssueResponse(resp *http.Response) (idempotentIssueResponse, error) {
	defer resp.Body.Close()
	var decoded idempotentIssueResponse
	err := json.NewDecoder(resp.Body).Decode(&decoded)
	return decoded, err
}

func cleanupIdempotentIssueTest(t *testing.T, workspaceID, key string, issueIDs ...string) {
	t.Helper()
	keyHash := strings.TrimPrefix(key, "sha256:")
	t.Cleanup(func() {
		ctx := context.Background()
		for _, issueID := range issueIDs {
			if issueID != "" {
				_, _ = testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1 AND workspace_id = $2`, issueID, workspaceID)
			}
		}
		_, _ = testPool.Exec(ctx, `DELETE FROM issue_create_idempotency WHERE workspace_id = $1 AND client_key_hash = $2`, workspaceID, keyHash)
		_, _ = testPool.Exec(ctx, `DELETE FROM workspace_event_outbox WHERE workspace_id = $1 AND aggregate_id = ANY($2::uuid[])`, workspaceID, issueIDs)
	})
}

func TestIssueCreateClientKeyReplaysAndRejectsChangedSemantics(t *testing.T) {
	if testPool == nil || testServer == nil {
		t.Skip("database unavailable")
	}
	seed := fmt.Sprintf("serial-%d", time.Now().UnixNano())
	key := testClientKey(seed)
	title := "idempotent serial " + seed
	secretDescription := "brief-must-not-be-stored-" + seed
	body := map[string]any{"client_key": key, "title": title, "description": secretDescription}

	first, err := postIssueForWorkspace(testWorkspaceID, body)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	firstDecoded, err := decodeIdempotentIssueResponse(first)
	if err != nil {
		t.Fatalf("decode first create: %v", err)
	}
	cleanupIdempotentIssueTest(t, testWorkspaceID, key, firstDecoded.ID)
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", first.StatusCode)
	}

	replay, err := postIssueForWorkspace(testWorkspaceID, body)
	if err != nil {
		t.Fatalf("replay create: %v", err)
	}
	replayDecoded, err := decodeIdempotentIssueResponse(replay)
	if err != nil {
		t.Fatalf("decode replay: %v", err)
	}
	if replay.StatusCode != http.StatusOK || replay.Header.Get("Idempotent-Replay") != "true" {
		t.Fatalf("replay status/header = %d/%q, want 200/true", replay.StatusCode, replay.Header.Get("Idempotent-Replay"))
	}
	if replayDecoded.ID != firstDecoded.ID {
		t.Fatalf("replay issue id = %s, want %s", replayDecoded.ID, firstDecoded.ID)
	}

	changed, err := postIssueForWorkspace(testWorkspaceID, map[string]any{"client_key": key, "title": title + " changed", "description": secretDescription})
	if err != nil {
		t.Fatalf("changed semantics create: %v", err)
	}
	changedDecoded, err := decodeIdempotentIssueResponse(changed)
	if err != nil {
		t.Fatalf("decode changed semantics response: %v", err)
	}
	if changed.StatusCode != http.StatusConflict || changedDecoded.Code != "issue_client_key_conflict" {
		t.Fatalf("changed semantics status/code = %d/%q, want 409/issue_client_key_conflict", changed.StatusCode, changedDecoded.Code)
	}

	var issueCount, createdEventCount int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM issue WHERE workspace_id = $1 AND id = $2`, testWorkspaceID, firstDecoded.ID).Scan(&issueCount); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM workspace_event_outbox WHERE workspace_id = $1 AND aggregate_id = $2 AND event_type = 'issue:created'`, testWorkspaceID, firstDecoded.ID).Scan(&createdEventCount); err != nil {
		t.Fatalf("count issue events: %v", err)
	}
	if issueCount != 1 || createdEventCount != 1 {
		t.Fatalf("issue/event counts = %d/%d, want 1/1", issueCount, createdEventCount)
	}

	var storedKeyHash, storedSemanticDigest string
	if err := testPool.QueryRow(context.Background(), `SELECT client_key_hash, semantic_digest FROM issue_create_idempotency WHERE workspace_id = $1 AND client_key_hash = $2`, testWorkspaceID, strings.TrimPrefix(key, "sha256:")).Scan(&storedKeyHash, &storedSemanticDigest); err != nil {
		t.Fatalf("read idempotency binding: %v", err)
	}
	if strings.Contains(storedKeyHash, secretDescription) || strings.Contains(storedSemanticDigest, secretDescription) || len(storedKeyHash) != 64 || len(storedSemanticDigest) != 64 {
		t.Fatalf("binding did not contain only fixed-size hashes: key=%q digest=%q", storedKeyHash, storedSemanticDigest)
	}

	if _, err := testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1 AND workspace_id = $2`, firstDecoded.ID, testWorkspaceID); err != nil {
		t.Fatalf("delete bound issue: %v", err)
	}
	deletedRetry, err := postIssueForWorkspace(testWorkspaceID, body)
	if err != nil {
		t.Fatalf("retry deleted target: %v", err)
	}
	deletedRetryDecoded, err := decodeIdempotentIssueResponse(deletedRetry)
	if err != nil {
		t.Fatalf("decode deleted-target response: %v", err)
	}
	if deletedRetry.StatusCode != http.StatusConflict || deletedRetryDecoded.Code != "issue_client_key_target_missing" {
		t.Fatalf("deleted-target status/code = %d/%q, want 409/issue_client_key_target_missing", deletedRetry.StatusCode, deletedRetryDecoded.Code)
	}
}

func TestIssueCreateClientKeyConcurrentRetryCreatesOneIssue(t *testing.T) {
	if testPool == nil || testServer == nil {
		t.Skip("database unavailable")
	}
	seed := fmt.Sprintf("concurrent-%d", time.Now().UnixNano())
	key := testClientKey(seed)
	body := map[string]any{"client_key": key, "title": "idempotent concurrent " + seed}

	type result struct {
		status int
		issue  idempotentIssueResponse
		err    error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := postIssueForWorkspace(testWorkspaceID, body)
			if err != nil {
				results <- result{err: err}
				return
			}
			decoded, decodeErr := decodeIdempotentIssueResponse(resp)
			results <- result{status: resp.StatusCode, issue: decoded, err: decodeErr}
		}()
	}
	wg.Wait()
	close(results)

	seenStatuses := map[int]int{}
	issueID := ""
	for got := range results {
		if got.err != nil {
			t.Fatalf("concurrent create: %v", got.err)
		}
		seenStatuses[got.status]++
		if issueID == "" {
			issueID = got.issue.ID
		} else if got.issue.ID != issueID {
			t.Fatalf("concurrent ids differ: %s != %s", got.issue.ID, issueID)
		}
	}
	cleanupIdempotentIssueTest(t, testWorkspaceID, key, issueID)
	if seenStatuses[http.StatusCreated] != 1 || seenStatuses[http.StatusOK] != 1 {
		t.Fatalf("concurrent statuses = %+v, want one 201 and one 200", seenStatuses)
	}
	var count int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM issue WHERE workspace_id = $1 AND id = $2`, testWorkspaceID, issueID).Scan(&count); err != nil {
		t.Fatalf("count concurrent issue: %v", err)
	}
	if count != 1 {
		t.Fatalf("concurrent issue count = %d, want 1", count)
	}
}

func TestIssueCreateClientKeyWorkspaceScopeAndMalformedKey(t *testing.T) {
	if testPool == nil || testServer == nil {
		t.Skip("database unavailable")
	}
	malformed, err := postIssueForWorkspace(testWorkspaceID, map[string]any{"client_key": "not-a-hash", "title": "never created"})
	if err != nil {
		t.Fatalf("malformed create: %v", err)
	}
	_, _ = io.Copy(io.Discard, malformed.Body)
	malformed.Body.Close()
	if malformed.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed status = %d, want 400", malformed.StatusCode)
	}

	ctx := context.Background()
	seed := fmt.Sprintf("workspace-%d", time.Now().UnixNano())
	key := testClientKey(seed)
	var otherWorkspaceID string
	if err := testPool.QueryRow(ctx, `INSERT INTO workspace (name, slug, description) VALUES ($1, $2, '') RETURNING id`, "Idempotency Scope", "idempotency-scope-"+seed).Scan(&otherWorkspaceID); err != nil {
		t.Fatalf("create other workspace: %v", err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`, otherWorkspaceID, testUserID); err != nil {
		t.Fatalf("create other workspace member: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, otherWorkspaceID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue_create_idempotency WHERE workspace_id = $1`, otherWorkspaceID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace_event_outbox WHERE workspace_id = $1`, otherWorkspaceID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace_event_cursor WHERE workspace_id = $1`, otherWorkspaceID)
	})

	first, err := postIssueForWorkspace(testWorkspaceID, map[string]any{"client_key": key, "title": "scope primary " + seed})
	if err != nil {
		t.Fatalf("primary workspace create: %v", err)
	}
	firstDecoded, err := decodeIdempotentIssueResponse(first)
	if err != nil || first.StatusCode != http.StatusCreated {
		t.Fatalf("primary workspace response = %d/%v", first.StatusCode, err)
	}
	cleanupIdempotentIssueTest(t, testWorkspaceID, key, firstDecoded.ID)

	second, err := postIssueForWorkspace(otherWorkspaceID, map[string]any{"client_key": key, "title": "scope other " + seed})
	if err != nil {
		t.Fatalf("other workspace create: %v", err)
	}
	secondDecoded, err := decodeIdempotentIssueResponse(second)
	if err != nil || second.StatusCode != http.StatusCreated {
		t.Fatalf("other workspace response = %d/%v", second.StatusCode, err)
	}
	if secondDecoded.ID == firstDecoded.ID {
		t.Fatalf("workspace-scoped key reused foreign issue %s", secondDecoded.ID)
	}
}

func TestIssueCreateClientKeyRejectsImmediateAgentDispatch(t *testing.T) {
	if testPool == nil || testServer == nil {
		t.Skip("database unavailable")
	}
	seed := fmt.Sprintf("assigned-%d", time.Now().UnixNano())
	key := testClientKey(seed)
	var agentID string
	if err := testPool.QueryRow(context.Background(), `SELECT id::text FROM agent WHERE workspace_id = $1 ORDER BY created_at LIMIT 1`, testWorkspaceID).Scan(&agentID); err != nil {
		t.Fatalf("load fixture agent: %v", err)
	}
	resp, err := postIssueForWorkspace(testWorkspaceID, map[string]any{
		"client_key": key, "title": "unsafe assigned " + seed,
		"assignee_type": "agent", "assignee_id": agentID, "status": "todo",
	})
	if err != nil {
		t.Fatalf("assigned create: %v", err)
	}
	decoded, err := decodeIdempotentIssueResponse(resp)
	if err != nil {
		t.Fatalf("decode assigned response: %v", err)
	}
	if resp.StatusCode != http.StatusUnprocessableEntity || decoded.Code != "issue_client_key_assigned_dispatch_unsupported" {
		t.Fatalf("assigned status/code = %d/%q, want 422/issue_client_key_assigned_dispatch_unsupported", resp.StatusCode, decoded.Code)
	}
	var issues, keys int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM issue WHERE workspace_id = $1 AND title = $2`, testWorkspaceID, "unsafe assigned "+seed).Scan(&issues); err != nil {
		t.Fatalf("count rejected issues: %v", err)
	}
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM issue_create_idempotency WHERE workspace_id = $1 AND client_key_hash = $2`, testWorkspaceID, strings.TrimPrefix(key, "sha256:")).Scan(&keys); err != nil {
		t.Fatalf("count rejected keys: %v", err)
	}
	if issues != 0 || keys != 0 {
		t.Fatalf("rejected assigned create left issue/key rows = %d/%d", issues, keys)
	}

	backlogKey := testClientKey(seed + "-backlog")
	backlogBody := map[string]any{
		"client_key": backlogKey, "title": "safe assigned backlog " + seed,
		"assignee_type": "agent", "assignee_id": agentID, "status": "backlog",
	}
	created, err := postIssueForWorkspace(testWorkspaceID, backlogBody)
	if err != nil {
		t.Fatalf("backlog create: %v", err)
	}
	createdIssue, err := decodeIdempotentIssueResponse(created)
	if err != nil || created.StatusCode != http.StatusCreated {
		t.Fatalf("backlog create status/error = %d/%v", created.StatusCode, err)
	}
	cleanupIdempotentIssueTest(t, testWorkspaceID, backlogKey, createdIssue.ID)
	replay, err := postIssueForWorkspace(testWorkspaceID, backlogBody)
	if err != nil {
		t.Fatalf("backlog replay: %v", err)
	}
	replayedIssue, err := decodeIdempotentIssueResponse(replay)
	if err != nil || replay.StatusCode != http.StatusOK || replayedIssue.ID != createdIssue.ID {
		t.Fatalf("backlog replay = status %d id %s err %v; want 200 id %s", replay.StatusCode, replayedIssue.ID, err, createdIssue.ID)
	}
	var tasks int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, createdIssue.ID).Scan(&tasks); err != nil {
		t.Fatalf("count backlog tasks: %v", err)
	}
	if tasks != 0 {
		t.Fatalf("client-key backlog create dispatched %d tasks", tasks)
	}
}
