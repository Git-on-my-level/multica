package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestParseWorkspaceEventCursor(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want int64
		bad  bool
	}{
		{name: "empty", raw: "", want: 0},
		{name: "zero", raw: "0", want: 0},
		{name: "positive", raw: "42", want: 42},
		{name: "negative", raw: "-1", bad: true},
		{name: "opaque garbage", raw: "cursor-1", bad: true},
		{name: "overflow", raw: "9223372036854775808", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseWorkspaceEventCursor(tc.raw, nil)
			if tc.bad && err == nil {
				t.Fatalf("parseWorkspaceEventCursor(%q) succeeded", tc.raw)
			}
			if !tc.bad && (err != nil || got != tc.want) {
				t.Fatalf("parseWorkspaceEventCursor(%q) = %d, %v; want %d", tc.raw, got, err, tc.want)
			}
		})
	}
}

func TestWorkspaceEventCursorBindsNormalizedFilters(t *testing.T) {
	types := []string{"comment:created", "issue:updated"}
	cursor := formatWorkspaceEventCursor(42, types)
	got, err := parseWorkspaceEventCursor(cursor, types)
	if err != nil || got != 42 {
		t.Fatalf("parse filtered cursor = %d, %v", got, err)
	}
	if _, err := parseWorkspaceEventCursor(cursor, []string{"task:completed"}); err == nil {
		t.Fatal("filtered cursor was accepted with a changed filter")
	}
	if _, err := parseWorkspaceEventCursor("42", types); err == nil {
		t.Fatal("non-bootstrap numeric cursor was accepted with a filter")
	}
}

func TestListWorkspaceEventsRejectsMalformedCursorBeforeQuery(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/events?cursor=not-a-sequence", nil)
	rec := httptest.NewRecorder()
	(&Handler{}).ListWorkspaceEvents(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestWorkspaceEventAppendOrderingReplayAndIsolation(t *testing.T) {
	if testPool == nil || testHandler == nil {
		t.Skip("database unavailable")
	}
	ctx := context.Background()
	workspaceID := newTestUUID(t)
	foreignWorkspaceID := newTestUUID(t)
	aggregateID := newTestUUID(t)
	foreignAggregateID := newTestUUID(t)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace_event_outbox WHERE workspace_id IN ($1, $2)`, workspaceID, foreignWorkspaceID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace_event_cursor WHERE workspace_id IN ($1, $2)`, workspaceID, foreignWorkspaceID)
	})

	firstID, firstSequence := appendTestWorkspaceEvent(t, workspaceID, "source-one", aggregateID)
	duplicateID, duplicateSequence := appendTestWorkspaceEvent(t, workspaceID, "source-one", aggregateID)
	if duplicateID != firstID || duplicateSequence != firstSequence {
		t.Fatalf("duplicate append = (%s, %d), want stable (%s, %d)", duplicateID, duplicateSequence, firstID, firstSequence)
	}
	if _, err := testPool.Exec(ctx, `
		SELECT append_workspace_event($1, 'source-one', 'comment:created', 'comment', $2, 'member', NULL, '{}'::jsonb, now())
	`, workspaceID, aggregateID); err == nil {
		t.Fatal("same source_id with changed event identity unexpectedly succeeded")
	}

	const concurrentAppends = 8
	var wg sync.WaitGroup
	errCh := make(chan error, concurrentAppends)
	for i := 0; i < concurrentAppends; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := testPool.Exec(ctx, `
				SELECT append_workspace_event($1, $2, 'task:running', 'task', $3, 'system', NULL, '{}'::jsonb, now())
			`, workspaceID, "concurrent-"+strconv.Itoa(i), aggregateID)
			errCh <- err
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent append: %v", err)
		}
	}
	appendTestWorkspaceEvent(t, foreignWorkspaceID, "foreign", foreignAggregateID)

	member := db.Member{WorkspaceID: parseUUID(workspaceID)}
	requestCtx := middleware.SetMemberContext(context.Background(), workspaceID, member)
	req := httptest.NewRequest(http.MethodGet, "/api/events?cursor="+strconv.FormatInt(firstSequence, 10)+"&limit=3", nil).WithContext(requestCtx)
	rec := httptest.NewRecorder()
	testHandler.ListWorkspaceEvents(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var page workspaceEventPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if len(page.Events) != 3 || !page.HasMore {
		t.Fatalf("page = %+v, want three events and has_more", page)
	}
	previous := firstSequence
	for _, event := range page.Events {
		if event.WorkspaceID != workspaceID {
			t.Fatalf("cross-workspace event leaked: %+v", event)
		}
		if event.AggregateID == foreignAggregateID {
			t.Fatalf("foreign aggregate leaked: %+v", event)
		}
		if event.Sequence <= previous {
			t.Fatalf("sequence %d after %d is not strictly increasing", event.Sequence, previous)
		}
		previous = event.Sequence
	}
	if page.NextCursor != strconv.FormatInt(previous, 10) {
		t.Fatalf("next_cursor = %q, want %d", page.NextCursor, previous)
	}
}

func TestWorkspaceEventFilteredReplayAndExpiredCursor(t *testing.T) {
	if testPool == nil || testHandler == nil {
		t.Skip("database unavailable")
	}
	workspaceID := newTestUUID(t)
	issueID := newTestUUID(t)
	taskID := newTestUUID(t)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace_event_outbox WHERE workspace_id = $1`, workspaceID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace_event_cursor WHERE workspace_id = $1`, workspaceID)
	})
	appendTestWorkspaceEvent(t, workspaceID, "issue-one", issueID)
	if _, err := testPool.Exec(context.Background(), `
		SELECT append_workspace_event($1, 'task-one', 'task:completed', 'task', $2, 'system', NULL, '{}'::jsonb, now())
	`, workspaceID, taskID); err != nil {
		t.Fatalf("append task event: %v", err)
	}
	appendTestWorkspaceEvent(t, workspaceID, "issue-two", issueID)

	member := db.Member{WorkspaceID: parseUUID(workspaceID)}
	requestCtx := middleware.SetMemberContext(context.Background(), workspaceID, member)
	req := httptest.NewRequest(http.MethodGet, "/api/events?cursor=0&type=task%3Acompleted", nil).WithContext(requestCtx)
	rec := httptest.NewRecorder()
	testHandler.ListWorkspaceEvents(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("filtered status = %d body=%s", rec.Code, rec.Body.String())
	}
	var page workspaceEventPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode filtered page: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].Type != "task:completed" {
		t.Fatalf("filtered events = %+v", page.Events)
	}
	filteredSequence, err := parseWorkspaceEventCursor(page.NextCursor, []string{"task:completed"})
	if err != nil {
		t.Fatalf("returned filtered cursor is invalid: %v", err)
	}
	if filteredSequence != 3 {
		t.Fatalf("filtered cursor sequence = %d, want workspace high-water 3", filteredSequence)
	}

	if _, err := testPool.Exec(context.Background(), `DELETE FROM workspace_event_outbox WHERE workspace_id = $1 AND sequence <= 2`, workspaceID); err != nil {
		t.Fatalf("prune retained prefix: %v", err)
	}
	expiredReq := httptest.NewRequest(http.MethodGet, "/api/events?cursor=1", nil).WithContext(requestCtx)
	expiredRec := httptest.NewRecorder()
	testHandler.ListWorkspaceEvents(expiredRec, expiredReq)
	if expiredRec.Code != http.StatusGone {
		t.Fatalf("expired status = %d body=%s", expiredRec.Code, expiredRec.Body.String())
	}
	var expired map[string]any
	if err := json.Unmarshal(expiredRec.Body.Bytes(), &expired); err != nil {
		t.Fatalf("decode expired response: %v", err)
	}
	if expired["code"] != "cursor_expired" || expired["oldest_cursor"] != "2" {
		t.Fatalf("expired response = %+v", expired)
	}

	aheadReq := httptest.NewRequest(http.MethodGet, "/api/events?cursor=999", nil).WithContext(requestCtx)
	aheadRec := httptest.NewRecorder()
	testHandler.ListWorkspaceEvents(aheadRec, aheadReq)
	if aheadRec.Code != http.StatusBadRequest {
		t.Fatalf("ahead-of-stream status = %d body=%s", aheadRec.Code, aheadRec.Body.String())
	}
}

func TestWorkspaceEventPruningIsBoundedAndDeletesOnlyEligiblePrefix(t *testing.T) {
	if testPool == nil || testHandler == nil {
		t.Skip("database unavailable")
	}
	ctx := context.Background()
	workspaceID := newTestUUID(t)
	aggregateID := newTestUUID(t)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace_event_outbox WHERE workspace_id = $1`, workspaceID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace_event_cursor WHERE workspace_id = $1`, workspaceID)
	})
	for i := 1; i <= 5; i++ {
		appendTestWorkspaceEvent(t, workspaceID, "retention-"+strconv.Itoa(i), aggregateID)
	}
	if _, err := testPool.Exec(ctx, `
		UPDATE workspace_event_outbox
		SET recorded_at = CASE
			WHEN sequence IN (1, 2, 4) THEN now() - interval '48 hours'
			ELSE now()
		END
		WHERE workspace_id = $1
	`, workspaceID); err != nil {
		t.Fatalf("age events: %v", err)
	}
	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-24 * time.Hour), Valid: true}
	workspaceUUID := parseUUID(workspaceID)
	count, err := testHandler.Queries.CountPrunableWorkspaceEvents(ctx, db.CountPrunableWorkspaceEventsParams{
		WorkspaceID: workspaceUUID, Cutoff: cutoff,
	})
	if err != nil || count != 2 {
		t.Fatalf("prunable count = %d, %v; want 2", count, err)
	}
	first, err := testHandler.Queries.PruneWorkspaceEventsBatch(ctx, db.PruneWorkspaceEventsBatchParams{
		WorkspaceID: workspaceUUID, Cutoff: cutoff, BatchSize: 1,
	})
	if err != nil || first.DeletedCount != 1 || first.FirstDeletedSequence != 1 || first.LastDeletedSequence != 1 {
		t.Fatalf("first prune = %+v, %v", first, err)
	}
	second, err := testHandler.Queries.PruneWorkspaceEventsBatch(ctx, db.PruneWorkspaceEventsBatchParams{
		WorkspaceID: workspaceUUID, Cutoff: cutoff, BatchSize: 10,
	})
	if err != nil || second.DeletedCount != 1 || second.FirstDeletedSequence != 2 || second.LastDeletedSequence != 2 {
		t.Fatalf("second prune = %+v, %v", second, err)
	}
	var remaining []int64
	rows, err := testPool.Query(ctx, `SELECT sequence FROM workspace_event_outbox WHERE workspace_id = $1 ORDER BY sequence`, workspaceID)
	if err != nil {
		t.Fatalf("list retained events: %v", err)
	}
	for rows.Next() {
		var sequence int64
		if err := rows.Scan(&sequence); err != nil {
			t.Fatalf("scan retained sequence: %v", err)
		}
		remaining = append(remaining, sequence)
	}
	rows.Close()
	if got := fmt.Sprint(remaining); got != "[3 4 5]" {
		t.Fatalf("remaining sequences = %s, want [3 4 5]", got)
	}
	bounds, err := testHandler.Queries.GetWorkspaceEventBounds(ctx, workspaceUUID)
	if err != nil || bounds.MinSequence != 3 || bounds.MaxSequence != 5 {
		t.Fatalf("bounds after prune = %+v, %v; want min=3 max=5", bounds, err)
	}
	// Sequence 4 is old but cannot be removed through the recent sequence 3;
	// making 3 eligible turns both into a contiguous, safe prefix.
	if _, err := testPool.Exec(ctx, `UPDATE workspace_event_outbox SET recorded_at = now() - interval '48 hours' WHERE workspace_id = $1 AND sequence = 3`, workspaceID); err != nil {
		t.Fatalf("age prefix blocker: %v", err)
	}
	third, err := testHandler.Queries.PruneWorkspaceEventsBatch(ctx, db.PruneWorkspaceEventsBatchParams{
		WorkspaceID: workspaceUUID, Cutoff: cutoff, BatchSize: 10,
	})
	if err != nil || third.DeletedCount != 2 || third.FirstDeletedSequence != 3 || third.LastDeletedSequence != 4 {
		t.Fatalf("third prune = %+v, %v", third, err)
	}

	member := db.Member{WorkspaceID: workspaceUUID}
	requestCtx := middleware.SetMemberContext(context.Background(), workspaceID, member)
	expiredReq := httptest.NewRequest(http.MethodGet, "/api/events?cursor=1", nil).WithContext(requestCtx)
	expiredRec := httptest.NewRecorder()
	testHandler.ListWorkspaceEvents(expiredRec, expiredReq)
	if expiredRec.Code != http.StatusGone {
		t.Fatalf("pruned cursor status = %d body=%s", expiredRec.Code, expiredRec.Body.String())
	}
}

func TestWorkspaceEventTriggersCoverAuthoritativeTransitionsAtomically(t *testing.T) {
	if testPool == nil {
		t.Skip("database unavailable")
	}
	ctx := context.Background()
	var agentID string
	if err := testPool.QueryRow(ctx, `SELECT id::text FROM agent WHERE workspace_id = $1 ORDER BY created_at LIMIT 1`, testWorkspaceID).Scan(&agentID); err != nil {
		t.Fatalf("load fixture agent: %v", err)
	}

	var issueID, commentID, taskID, autopilotID, runID, artifactID string
	secretComment := "secret comment body must not enter the outbox"
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, creator_type, creator_id)
		VALUES ($1, 'workspace event trigger coverage', 'member', $2)
		RETURNING id::text
	`, testWorkspaceID, testUserID).Scan(&issueID); err != nil {
		t.Fatalf("insert issue: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM attachment WHERE id = $1`, artifactID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM autopilot_run WHERE id = $1`, runID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM autopilot WHERE id = $1`, autopilotID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM comment WHERE id = $1`, commentID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace_event_outbox WHERE aggregate_id = ANY($1::uuid[])`, []string{issueID, commentID, taskID, runID, artifactID})
	})

	if err := testPool.QueryRow(ctx, `
		INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content)
		VALUES ($1, $2, 'member', $3, $4)
		RETURNING id::text
	`, testWorkspaceID, issueID, testUserID, secretComment).Scan(&commentID); err != nil {
		t.Fatalf("insert comment: %v", err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (
			agent_id, issue_id, runtime_id, status, initiator_user_id,
			originator_user_id, accountable_user_id, originator_source
		) VALUES ($1, $2, $3, 'queued', $4, $4, $4, 'direct_human')
		RETURNING id::text
	`, agentID, issueID, testRuntimeID, testUserID).Scan(&taskID); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, taskID); err != nil {
		t.Fatalf("start task: %v", err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO autopilot (
			workspace_id, title, assignee_id, assignee_type, execution_mode,
			created_by_type, created_by_id
		) VALUES ($1, 'workspace event autopilot', $2, 'agent', 'run_only', 'member', $3)
		RETURNING id::text
	`, testWorkspaceID, agentID, testUserID).Scan(&autopilotID); err != nil {
		t.Fatalf("insert autopilot: %v", err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO autopilot_run (autopilot_id, source, status)
		VALUES ($1, 'manual', 'running')
		RETURNING id::text
	`, autopilotID).Scan(&runID); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE autopilot_run SET status = 'completed', completed_at = now() WHERE id = $1`, runID); err != nil {
		t.Fatalf("complete run: %v", err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO attachment (
			workspace_id, issue_id, uploader_type, uploader_id, filename,
			url, content_type, size_bytes, task_id
		) VALUES ($1, $2, 'member', $3, 'result.txt', 's3://private/secret', 'text/plain', 12, $4)
		RETURNING id::text
	`, testWorkspaceID, issueID, testUserID, taskID).Scan(&artifactID); err != nil {
		t.Fatalf("insert artifact: %v", err)
	}

	expected := map[string]string{
		issueID + ":issue:created":       "issue",
		commentID + ":comment:created":   "comment",
		taskID + ":task:queued":          "task",
		taskID + ":task:running":         "task",
		runID + ":run:running":           "run",
		runID + ":run:completed":         "run",
		artifactID + ":artifact:created": "artifact",
	}
	rows, err := testPool.Query(ctx, `
		SELECT aggregate_id::text, event_type, aggregate_kind, payload::text
		FROM workspace_event_outbox
		WHERE aggregate_id = ANY($1::uuid[])
	`, []string{issueID, commentID, taskID, runID, artifactID})
	if err != nil {
		t.Fatalf("list captured events: %v", err)
	}
	defer rows.Close()
	seen := make(map[string]bool)
	for rows.Next() {
		var aggregateID, eventType, aggregateKind, payload string
		if err := rows.Scan(&aggregateID, &eventType, &aggregateKind, &payload); err != nil {
			t.Fatalf("scan captured event: %v", err)
		}
		key := aggregateID + ":" + eventType
		if wantKind, ok := expected[key]; ok {
			if aggregateKind != wantKind {
				t.Fatalf("%s aggregate_kind = %q, want %q", key, aggregateKind, wantKind)
			}
			seen[key] = true
		}
		if strings.Contains(payload, secretComment) || strings.Contains(payload, "s3://private/secret") {
			t.Fatalf("sensitive body or storage URL leaked into event payload: %s", payload)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate captured events: %v", err)
	}
	for key := range expected {
		if !seen[key] {
			t.Errorf("missing captured transition %s", key)
		}
	}

	rolledBackID := newTestUUID(t)
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin rollback transaction: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO issue (id, workspace_id, title, creator_type, creator_id, number)
		VALUES (
			$1, $2, 'rolled back issue', 'member', $3,
			(SELECT COALESCE(MAX(number), 0) + 1 FROM issue WHERE workspace_id = $2)
		)
	`, rolledBackID, testWorkspaceID, testUserID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("insert rolled-back issue: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	var rolledBackEvents int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM workspace_event_outbox WHERE aggregate_id = $1`, rolledBackID).Scan(&rolledBackEvents); err != nil {
		t.Fatalf("count rolled-back events: %v", err)
	}
	if rolledBackEvents != 0 {
		t.Fatalf("rolled-back mutation left %d durable events", rolledBackEvents)
	}
}

func newTestUUID(t *testing.T) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(), `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatalf("generate UUID: %v", err)
	}
	return id
}

func appendTestWorkspaceEvent(t *testing.T, workspaceID, sourceID, aggregateID string) (string, int64) {
	t.Helper()
	var id string
	var sequence int64
	if err := testPool.QueryRow(context.Background(), `
		SELECT id::text, sequence
		FROM append_workspace_event($1, $2, 'issue:updated', 'issue', $3, 'member', NULL, '{}'::jsonb, now())
	`, workspaceID, sourceID, aggregateID).Scan(&id, &sequence); err != nil {
		t.Fatalf("append workspace event: %v", err)
	}
	return id, sequence
}
