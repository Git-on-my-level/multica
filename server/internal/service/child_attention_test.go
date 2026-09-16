package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// childAttentionFixture is deliberately built through the shared DB fixtures.
// The status transition itself uses the production SQL trigger, so these tests
// cover both the writer transaction and the recovery worker rather than a
// private helper that bypasses the database contract.
type childAttentionFixture struct {
	pool      *pgxpool.Pool
	fx        *dbfx.Fixture
	userID    string
	runtimeID string
	agentID   string
	parentID  string
	service   *TaskService
}

func newChildAttentionFixture(t *testing.T) *childAttentionFixture {
	t.Helper()
	pool := newResolveOriginatorPool(t)
	base := dbfx.New(pool, "", "")
	suffix := time.Now().UnixNano()
	userID := base.User(t, fmt.Sprintf("Child attention user %d", suffix), fmt.Sprintf("child-attention-%d@multica.test", suffix))
	workspaceID := base.Workspace(t, fmt.Sprintf("Child attention workspace %d", suffix), fmt.Sprintf("child-attention-%d", suffix))
	base.Member(t, workspaceID, userID, "owner")
	fx := dbfx.New(pool, workspaceID, userID)
	runtimeID := fx.Runtime(t, fmt.Sprintf("child-attention-runtime-%d", suffix))
	agentID := fx.Agent(t, fmt.Sprintf("child-attention-agent-%d", suffix), runtimeID)
	parentID := fx.Issue(t, "parent", dbfx.Cols{
		"assignee_type": "agent",
		"assignee_id":   agentID,
	})

	// Recovery-created comments and tasks are not fixture rows. Remove them
	// before the fixture's issue/agent/workspace cleanup runs.
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id = $1`, parentID)
		_, _ = pool.Exec(ctx, `DELETE FROM comment WHERE issue_id = $1`, parentID)
		_, _ = pool.Exec(ctx, `DELETE FROM issue_child_attention WHERE parent_id = $1`, parentID)
	})

	return &childAttentionFixture{
		pool: pool, fx: fx, userID: userID, runtimeID: runtimeID, agentID: agentID, parentID: parentID,
		service: NewTaskService(db.New(pool), pool, nil, nil),
	}
}

func (f *childAttentionFixture) child(t *testing.T, status string) string {
	t.Helper()
	childID := f.fx.Issue(t, "child", dbfx.Cols{"parent_issue_id": f.parentID})
	f.setChildStatus(t, childID, status)
	return childID
}

func (f *childAttentionFixture) setChildStatus(t *testing.T, childID, status string) {
	t.Helper()
	f.fx.Exec(t, `UPDATE issue SET status = $2 WHERE id = $1`, childID, status)
	// The trigger intentionally uses a short delay to avoid doing work in the
	// status writer's request. Tests own the clock by making the captured row
	// due immediately; no sleep is needed and restart tests stay deterministic.
	f.fx.Exec(t, `UPDATE issue_child_attention SET available_at = now() - interval '1 second' WHERE child_id = $1`, childID)
}

func (f *childAttentionFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query: %v", err)
	}
	return n
}

func (f *childAttentionFixture) signalCount(t *testing.T) int {
	return f.count(t, `SELECT count(*) FROM issue_child_attention WHERE parent_id = $1`, f.parentID)
}

func (f *childAttentionFixture) commentCount(t *testing.T) int {
	return f.count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system' AND type = 'system'`, f.parentID)
}

func (f *childAttentionFixture) taskCount(t *testing.T) int {
	return f.count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, f.parentID)
}

func (f *childAttentionFixture) forceDue(t *testing.T) {
	t.Helper()
	f.fx.Exec(t, `UPDATE issue_child_attention SET available_at = now() - interval '1 second' WHERE parent_id = $1`, f.parentID)
}

func TestChildAttentionStatusCaptureRollsBackWithWriterTransaction(t *testing.T) {
	f := newChildAttentionFixture(t)
	ctx := context.Background()
	childID := f.fx.Issue(t, "transactional child", dbfx.Cols{"parent_issue_id": f.parentID})
	childUUID := util.MustParseUUID(childID)

	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE issue SET status = 'in_review' WHERE id = $1`, childID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM issue WHERE id = $1`, childID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "todo" || f.signalCount(t) != 0 {
		t.Fatalf("rolled back status = %q, signals = %d; want todo and no signal", status, f.signalCount(t))
	}

	tx, err = f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE issue SET status = 'blocked' WHERE id = $1`, childID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	f.forceDue(t)
	var capturedStatus string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM issue_child_attention WHERE child_id = $1`, childUUID).Scan(&capturedStatus); err != nil {
		t.Fatal(err)
	}
	if capturedStatus != "blocked" {
		t.Fatalf("captured status = %q, want blocked", capturedStatus)
	}
}

func TestChildAttentionRecoveryCoalescesSiblingSignals(t *testing.T) {
	f := newChildAttentionFixture(t)
	f.child(t, "in_review")
	f.child(t, "blocked")

	queued, err := f.service.RecoverChildAttention(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if queued != 1 || f.signalCount(t) != 0 || f.commentCount(t) != 1 || f.taskCount(t) != 1 {
		t.Fatalf("recovery queued=%d signals=%d comments=%d tasks=%d; want 1/0/1/1", queued, f.signalCount(t), f.commentCount(t), f.taskCount(t))
	}

	var content string
	if err := f.pool.QueryRow(context.Background(), `SELECT content FROM comment WHERE issue_id = $1 AND author_type = 'system' AND type = 'system'`, f.parentID).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if want := "in_review"; !strings.Contains(content, want) {
		t.Fatalf("coalesced comment = %q; want status %q", content, want)
	}
	if want := "blocked"; !strings.Contains(content, want) {
		t.Fatalf("coalesced comment = %q; want status %q", content, want)
	}
}

func TestChildAttentionDistinctHandoffSurvivesExistingAssignment(t *testing.T) {
	for _, status := range []string{"queued", "dispatched", "running"} {
		t.Run(status, func(t *testing.T) {
			f := newChildAttentionFixture(t)
			f.fx.Task(t, f.agentID, dbfx.Cols{"runtime_id": f.runtimeID, "issue_id": f.parentID, "status": status})
			f.child(t, "in_review")
			if queued, err := f.service.RecoverChildAttention(context.Background(), 100); err != nil || queued != 1 {
				t.Fatalf("recovery queued=%d err=%v; want one distinct handoff", queued, err)
			}
			if f.commentCount(t) != 1 || f.taskCount(t) != 2 || f.signalCount(t) != 0 {
				t.Fatalf("existing %s assignment swallowed handoff: comments=%d tasks=%d signals=%d; want 1/2/0", status, f.commentCount(t), f.taskCount(t), f.signalCount(t))
			}
		})
	}
}

func TestChildAttentionRecoveryIsIdempotentAcrossServiceRestart(t *testing.T) {
	f := newChildAttentionFixture(t)
	f.child(t, "in_review")
	if queued, err := f.service.RecoverChildAttention(context.Background(), 100); err != nil || queued != 1 {
		t.Fatalf("initial recovery queued=%d err=%v", queued, err)
	}
	newService := NewTaskService(db.New(f.pool), f.pool, nil, nil)
	if queued, err := newService.RecoverChildAttention(context.Background(), 100); err != nil || queued != 0 {
		t.Fatalf("restart recovery queued=%d err=%v; want no duplicate", queued, err)
	}
	if f.commentCount(t) != 1 || f.taskCount(t) != 1 {
		t.Fatalf("restart duplicated delivery: comments=%d tasks=%d; want 1/1", f.commentCount(t), f.taskCount(t))
	}
}

func TestChildAttentionReopenCreatesFreshSignalAndSameStatusDoesNot(t *testing.T) {
	f := newChildAttentionFixture(t)
	childID := f.child(t, "in_review")
	if f.signalCount(t) != 1 {
		t.Fatalf("initial signals = %d, want 1", f.signalCount(t))
	}
	// A repeated status write is a no-op for attention capture.
	f.fx.Exec(t, `UPDATE issue SET status = 'in_review' WHERE id = $1`, childID)
	if f.signalCount(t) != 1 {
		t.Fatalf("repeated in_review write created another signal: %d", f.signalCount(t))
	}
	if queued, err := f.service.RecoverChildAttention(context.Background(), 100); err != nil || queued != 1 {
		t.Fatalf("initial delivery queued=%d err=%v", queued, err)
	}
	f.fx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE issue_id = $1`, f.parentID)
	f.setChildStatus(t, childID, "todo")
	if f.signalCount(t) != 0 {
		t.Fatalf("reopen to todo retained old signal: %d", f.signalCount(t))
	}
	f.setChildStatus(t, childID, "in_review")
	if f.signalCount(t) != 1 {
		t.Fatalf("reopen to in_review signals = %d, want fresh signal", f.signalCount(t))
	}
	if queued, err := f.service.RecoverChildAttention(context.Background(), 100); err != nil || queued != 1 {
		t.Fatalf("fresh delivery queued=%d err=%v", queued, err)
	}
	if f.commentCount(t) != 2 || f.taskCount(t) != 2 {
		t.Fatalf("fresh handoff was coalesced away: comments=%d tasks=%d; want 2/2", f.commentCount(t), f.taskCount(t))
	}
}

func TestChildAttentionOldGenerationRetryCannotDelayFreshTransition(t *testing.T) {
	f := newChildAttentionFixture(t)
	childID := f.child(t, "in_review")
	ctx := context.Background()
	childUUID := util.MustParseUUID(childID)
	var oldGeneration pgtype.UUID
	if err := f.pool.QueryRow(ctx, `SELECT generation FROM issue_child_attention WHERE child_id = $1`, childID).Scan(&oldGeneration); err != nil {
		t.Fatal(err)
	}

	// Replace the signal with a new immutable generation for the same child.
	f.setChildStatus(t, childID, "todo")
	f.setChildStatus(t, childID, "in_review")
	var freshGeneration pgtype.UUID
	if err := f.pool.QueryRow(ctx, `SELECT generation FROM issue_child_attention WHERE child_id = $1`, childID).Scan(&freshGeneration); err != nil {
		t.Fatal(err)
	}
	if freshGeneration == oldGeneration {
		t.Fatal("fresh child transition reused the old attention generation")
	}

	var before time.Time
	if err := f.pool.QueryRow(ctx, `SELECT available_at FROM issue_child_attention WHERE child_id = $1`, childID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := db.New(f.pool).DeferChildAttention(ctx, db.DeferChildAttentionParams{
		ChildIds: []pgtype.UUID{childUUID}, Generations: []pgtype.UUID{oldGeneration},
	}); err != nil {
		t.Fatal(err)
	}
	var after time.Time
	if err := f.pool.QueryRow(ctx, `SELECT available_at FROM issue_child_attention WHERE child_id = $1`, childID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) {
		t.Fatalf("old-generation defer changed fresh available_at from %s to %s", before, after)
	}
}

func TestChildAttentionBlockedTransition(t *testing.T) {
	f := newChildAttentionFixture(t)
	childID := f.child(t, "blocked")
	var status string
	if err := f.pool.QueryRow(context.Background(), `SELECT status FROM issue_child_attention WHERE child_id = $1`, childID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "blocked" {
		t.Fatalf("blocked signal status = %q, want blocked", status)
	}
}

func TestChildAttentionSkipsParkedOrHumanParents(t *testing.T) {
	for _, tc := range []struct {
		name       string
		parentStat string
		member     bool
	}{
		{name: "backlog", parentStat: "backlog"},
		{name: "done", parentStat: "done"},
		{name: "cancelled", parentStat: "cancelled"},
		{name: "member", parentStat: "todo", member: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newChildAttentionFixture(t)
			if tc.member {
				f.fx.Exec(t, `UPDATE issue SET assignee_type = 'member', assignee_id = $2 WHERE id = $1`, f.parentID, f.userID)
			}
			f.fx.Exec(t, `UPDATE issue SET status = $2 WHERE id = $1`, f.parentID, tc.parentStat)
			f.child(t, "in_review")
			if queued, err := f.service.RecoverChildAttention(context.Background(), 100); err != nil || queued != 0 {
				t.Fatalf("parked/human recovery queued=%d err=%v; want zero", queued, err)
			}
			if f.signalCount(t) != 0 || f.commentCount(t) != 0 || f.taskCount(t) != 0 {
				t.Fatalf("parked/human parent delivered work: signals=%d comments=%d tasks=%d; want 0/0/0", f.signalCount(t), f.commentCount(t), f.taskCount(t))
			}
		})
	}
}

func TestChildAttentionParkedParentPromotionDoesNotReplay(t *testing.T) {
	f := newChildAttentionFixture(t)
	// Capture is intentionally suppressed while a parent is parked. Promoting
	// the parent before the worker runs must not turn that old child state into
	// a new assignment; a subsequent child transition is the explicit signal.
	f.fx.Exec(t, `UPDATE issue SET status = 'backlog' WHERE id = $1`, f.parentID)
	f.child(t, "in_review")
	if got := f.signalCount(t); got != 0 {
		t.Fatalf("parked parent captured child signal: %d", got)
	}
	f.fx.Exec(t, `UPDATE issue SET status = 'todo' WHERE id = $1`, f.parentID)
	f.forceDue(t)
	if queued, err := f.service.RecoverChildAttention(context.Background(), 100); err != nil || queued != 0 {
		t.Fatalf("promoted parked parent recovery queued=%d err=%v; want no replay", queued, err)
	}
	if f.commentCount(t) != 0 || f.taskCount(t) != 0 {
		t.Fatalf("promoted parked parent replayed work: comments=%d tasks=%d; want 0/0", f.commentCount(t), f.taskCount(t))
	}
}

func TestChildAttentionSquadTargetsOnlyLeader(t *testing.T) {
	f := newChildAttentionFixture(t)
	squadID := f.fx.Squad(t, "coordination squad", f.agentID)
	workerID := f.fx.Agent(t, "squad-worker", f.runtimeID)
	f.fx.SquadMember(t, squadID, "agent", workerID)
	f.fx.Exec(t, `UPDATE issue SET assignee_type = 'squad', assignee_id = $2 WHERE id = $1`, f.parentID, squadID)
	f.child(t, "in_review")
	if queued, err := f.service.RecoverChildAttention(context.Background(), 100); err != nil || queued != 1 {
		t.Fatalf("squad recovery queued=%d err=%v; want one leader task", queued, err)
	}
	var agentID, gotSquad string
	var leader bool
	if err := f.pool.QueryRow(context.Background(), `SELECT agent_id, squad_id, is_leader_task FROM agent_task_queue WHERE issue_id = $1`, f.parentID).Scan(&agentID, &gotSquad, &leader); err != nil {
		t.Fatal(err)
	}
	if agentID != f.agentID || gotSquad != squadID || !leader {
		t.Fatalf("squad handoff target = %s/%s/leader=%v; want %s/%s/leader=true", agentID, gotSquad, leader, f.agentID, squadID)
	}
}

func TestChildAttentionArchivedOwnerDefersAndRetries(t *testing.T) {
	f := newChildAttentionFixture(t)
	f.child(t, "in_review")
	f.fx.Exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, f.agentID)
	if queued, err := f.service.RecoverChildAttention(context.Background(), 100); err != nil || queued != 0 {
		t.Fatalf("archived recovery queued=%d err=%v; want deferred zero", queued, err)
	}
	if f.signalCount(t) != 1 || f.commentCount(t) != 0 || f.taskCount(t) != 0 {
		t.Fatalf("archived owner lost handoff: signals=%d comments=%d tasks=%d; want 1/0/0", f.signalCount(t), f.commentCount(t), f.taskCount(t))
	}
	f.fx.Exec(t, `UPDATE agent SET archived_at = NULL WHERE id = $1`, f.agentID)
	f.forceDue(t)
	if queued, err := f.service.RecoverChildAttention(context.Background(), 100); err != nil || queued != 1 {
		t.Fatalf("retry recovery queued=%d err=%v; want one delivery", queued, err)
	}
	if f.signalCount(t) != 0 || f.commentCount(t) != 1 || f.taskCount(t) != 1 {
		t.Fatalf("retry delivery counts signals=%d comments=%d tasks=%d; want 0/1/1", f.signalCount(t), f.commentCount(t), f.taskCount(t))
	}
}

func TestChildAttentionWorkspaceLockOrderDoesNotDeadlockDeletion(t *testing.T) {
	f := newChildAttentionFixture(t)
	f.child(t, "in_review")
	ctx := context.Background()

	deleteTx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer deleteTx.Rollback(ctx)
	if _, err := deleteTx.Exec(ctx, `SELECT id FROM workspace WHERE id = $1 FOR UPDATE`, f.fx.WorkspaceID); err != nil {
		t.Fatal(err)
	}

	workerCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	workerDone := make(chan struct {
		queued int
		err    error
	}, 1)
	go func() {
		queued, err := f.service.RecoverChildAttention(workerCtx, 100)
		workerDone <- struct {
			queued int
			err    error
		}{queued: queued, err: err}
	}()

	// The worker must be waiting on its workspace FOR KEY SHARE before the
	// deletion transaction attempts the parent lock. pg_blocking_pids proves
	// this is the intended lock wait, rather than an arbitrary scheduler delay.
	deadline := time.Now().Add(3 * time.Second)
	for {
		var blocked int
		if err := f.pool.QueryRow(ctx, `
			SELECT count(*)
			FROM pg_stat_activity a
			WHERE a.pid <> pg_backend_pid()
			  AND a.wait_event_type = 'Lock'
			  AND a.query ILIKE '%FROM workspace w JOIN issue i%'
			  AND cardinality(pg_blocking_pids(a.pid)) > 0`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked > 0 {
			break
		}
		select {
		case result := <-workerDone:
			t.Fatalf("worker completed before workspace lock wait: queued=%d err=%v", result.queued, result.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("worker never reached the workspace lock wait")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A correct workspace-first order leaves the parent row available to the
	// deletion transaction. Locking it succeeds while the worker is blocked;
	// rollback releases the workspace and lets recovery finish.
	if _, err := deleteTx.Exec(ctx, `SELECT id FROM issue WHERE id = $1 FOR UPDATE`, f.parentID); err != nil {
		t.Fatalf("deletion parent lock deadlocked: %v", err)
	}
	if err := deleteTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	result := <-workerDone
	if result.err != nil || result.queued != 1 {
		t.Fatalf("worker after deletion rollback: queued=%d err=%v; want one delivery", result.queued, result.err)
	}
	if f.commentCount(t) != 1 || f.taskCount(t) != 1 || f.signalCount(t) != 0 {
		t.Fatalf("post-lock-order delivery counts comments=%d tasks=%d signals=%d; want 1/1/0", f.commentCount(t), f.taskCount(t), f.signalCount(t))
	}
}

func TestChildAttentionRuntimeLockOrderDoesNotDeadlockDeletion(t *testing.T) {
	f := newChildAttentionFixture(t)
	f.child(t, "in_review")
	ctx := context.Background()

	deleteTx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer deleteTx.Rollback(ctx)
	if _, err := deleteTx.Exec(ctx, `SELECT id FROM agent_runtime WHERE id = $1 FOR UPDATE`, f.runtimeID); err != nil {
		t.Fatal(err)
	}
	// If the recovery worker locked the agent before waiting for the runtime,
	// this transaction's agent lock below would wait. Bound that assertion so a
	// lock-order regression fails promptly and cannot hang the test process.
	if _, err := deleteTx.Exec(ctx, `SET LOCAL lock_timeout = '500ms'`); err != nil {
		t.Fatal(err)
	}

	workerCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	workerDone := make(chan struct {
		queued int
		err    error
	}, 1)
	go func() {
		queued, err := f.service.RecoverChildAttention(workerCtx, 100)
		workerDone <- struct {
			queued int
			err    error
		}{queued: queued, err: err}
	}()

	// The runtime pre-lock is the first lock that can conflict with teardown.
	// Wait for that exact query to become blocked, with PostgreSQL identifying
	// the blocker; a scheduler delay alone cannot satisfy this condition.
	deadline := time.Now().Add(3 * time.Second)
	for {
		var blocked int
		if err := f.pool.QueryRow(ctx, `
			SELECT count(*)
			FROM pg_stat_activity a
			WHERE a.pid <> pg_backend_pid()
			  AND a.wait_event_type = 'Lock'
			  AND a.query ILIKE '%agent_runtime%'
			  AND cardinality(pg_blocking_pids(a.pid)) > 0`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked > 0 {
			break
		}
		select {
		case result := <-workerDone:
			t.Fatalf("worker completed before runtime lock wait: queued=%d err=%v", result.queued, result.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("worker never reached the runtime lock wait")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Teardown must still be able to acquire the agent row while recovery is
	// waiting on the runtime. This proves the worker did not acquire agent first.
	if _, err := deleteTx.Exec(ctx, `SELECT id FROM agent WHERE id = $1 FOR UPDATE`, f.agentID); err != nil {
		t.Fatalf("deletion agent lock was blocked by recovery: %v", err)
	}
	if err := deleteTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	result := <-workerDone
	if result.err != nil || result.queued != 1 {
		t.Fatalf("worker after runtime deletion rollback: queued=%d err=%v; want one delivery", result.queued, result.err)
	}
	if f.commentCount(t) != 1 || f.taskCount(t) != 1 || f.signalCount(t) != 0 {
		t.Fatalf("post-runtime-lock delivery counts comments=%d tasks=%d signals=%d; want 1/1/0", f.commentCount(t), f.taskCount(t), f.signalCount(t))
	}
}

func TestChildAttentionAgentReassignmentContentionReturnsWithoutDelivery(t *testing.T) {
	f := newChildAttentionFixture(t)
	f.child(t, "in_review")
	ctx := context.Background()

	// FOR NO KEY UPDATE is the lock taken by an agent runtime reassignment. It
	// conflicts with the recovery worker's FOR SHARE NOWAIT and therefore must
	// not leave a recovery transaction waiting behind an administrative write.
	reassignTx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reassignTx.Rollback(ctx)
	if _, err := reassignTx.Exec(ctx, `SELECT id FROM agent WHERE id = $1 FOR NO KEY UPDATE`, f.agentID); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	workerCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	queued, err := f.service.RecoverChildAttention(workerCtx, 100)
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("recovery waited %s on agent reassignment lock; NOWAIT should return promptly", elapsed)
	}
	if err != nil || queued != 0 {
		t.Fatalf("contended recovery queued=%d err=%v; want no delivery", queued, err)
	}
	if f.commentCount(t) != 0 || f.taskCount(t) != 0 || f.signalCount(t) != 1 {
		t.Fatalf("contended recovery partial state: comments=%d tasks=%d signals=%d; want 0/0/1", f.commentCount(t), f.taskCount(t), f.signalCount(t))
	}

	if err := reassignTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	f.forceDue(t)
	if queued, err := f.service.RecoverChildAttention(ctx, 100); err != nil || queued != 1 {
		t.Fatalf("recovery after reassignment rollback queued=%d err=%v; want one delivery", queued, err)
	}
	if f.commentCount(t) != 1 || f.taskCount(t) != 1 || f.signalCount(t) != 0 {
		t.Fatalf("post-reassignment delivery counts comments=%d tasks=%d signals=%d; want 1/1/0", f.commentCount(t), f.taskCount(t), f.signalCount(t))
	}
}

func TestChildAttentionRollsBackCommentWhenTaskDeliveryFails(t *testing.T) {
	f := newChildAttentionFixture(t)
	f.child(t, "in_review")
	ctx := context.Background()

	// Fail only task inserts for this fixture's parent. The trigger is scoped to
	// this test's UUID and is removed before fixture cleanup, so this exercises
	// the production transaction without changing the schema or global test
	// behavior.
	failureFunction := fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION test_child_attention_fail_task_insert() RETURNS trigger
		LANGUAGE plpgsql AS $fn$
		BEGIN
			IF NEW.issue_id = '%s'::uuid THEN
				RAISE EXCEPTION 'test child attention task insert failure';
			END IF;
			RETURN NEW;
		END
		$fn$`, f.parentID)
	if _, err := f.pool.Exec(ctx, failureFunction); err != nil {
		t.Fatalf("install failure function: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		CREATE TRIGGER test_child_attention_fail_task
		BEFORE INSERT ON agent_task_queue
		FOR EACH ROW EXECUTE FUNCTION test_child_attention_fail_task_insert()`); err != nil {
		t.Fatalf("install failure trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS test_child_attention_fail_task ON agent_task_queue`)
		_, _ = f.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS test_child_attention_fail_task_insert()`)
	})

	if queued, err := f.service.RecoverChildAttention(ctx, 100); err != nil || queued != 0 {
		t.Fatalf("failed delivery queued=%d err=%v; want deferred zero", queued, err)
	}
	if f.commentCount(t) != 0 || f.taskCount(t) != 0 || f.signalCount(t) != 1 {
		t.Fatalf("failed delivery left partial state: comments=%d tasks=%d signals=%d; want 0/0/1", f.commentCount(t), f.taskCount(t), f.signalCount(t))
	}
	var availableAt time.Time
	if err := f.pool.QueryRow(ctx, `SELECT available_at FROM issue_child_attention WHERE parent_id = $1`, f.parentID).Scan(&availableAt); err != nil {
		t.Fatal(err)
	}
	if !availableAt.After(time.Now()) {
		t.Fatalf("failed delivery available_at = %s; want deferred retry", availableAt)
	}

	if _, err := f.pool.Exec(ctx, `DROP TRIGGER test_child_attention_fail_task ON agent_task_queue`); err != nil {
		t.Fatal(err)
	}
	f.forceDue(t)
	if queued, err := f.service.RecoverChildAttention(ctx, 100); err != nil || queued != 1 {
		t.Fatalf("retry delivery queued=%d err=%v; want one delivery", queued, err)
	}
	if f.commentCount(t) != 1 || f.taskCount(t) != 1 || f.signalCount(t) != 0 {
		t.Fatalf("retry delivery counts comments=%d tasks=%d signals=%d; want 1/1/0", f.commentCount(t), f.taskCount(t), f.signalCount(t))
	}
}

func TestChildAttentionDeletesStaleSignalsOnReparentAndDeletion(t *testing.T) {
	f := newChildAttentionFixture(t)
	otherParentID := f.fx.Issue(t, "other parent", dbfx.Cols{
		"assignee_type": "agent",
		"assignee_id":   f.agentID,
	})
	childID := f.child(t, "in_review")

	// Moving a child invalidates the old parent's durable signal. A status
	// transition under the new parent is the fresh event that should notify it.
	f.fx.Exec(t, `UPDATE issue SET parent_issue_id = $2 WHERE id = $1`, childID, otherParentID)
	if got := f.signalCount(t); got != 0 {
		t.Fatalf("old parent retained signal after reparent: %d", got)
	}
	if got := f.count(t, `SELECT count(*) FROM issue_child_attention WHERE parent_id = $1`, otherParentID); got != 0 {
		t.Fatalf("reparent created stale new-parent signal: %d", got)
	}
	f.setChildStatus(t, childID, "blocked")
	if got := f.count(t, `SELECT count(*) FROM issue_child_attention WHERE parent_id = $1`, otherParentID); got != 1 {
		t.Fatalf("new-parent transition signals = %d, want 1", got)
	}

	// Deleting the child must clear its durable obligation rather than leave a
	// worker to render a missing issue later.
	f.fx.Exec(t, `DELETE FROM issue WHERE id = $1`, childID)
	if got := f.count(t, `SELECT count(*) FROM issue_child_attention WHERE child_id = $1`, childID); got != 0 {
		t.Fatalf("child deletion retained signal: %d", got)
	}

	// Deleting a parent also clears any outstanding child obligations, even
	// though this table intentionally has no database foreign key.
	remainingChildID := f.child(t, "in_review")
	if got := f.count(t, `SELECT count(*) FROM issue_child_attention WHERE parent_id = $1`, f.parentID); got != 1 {
		t.Fatalf("parent signal before parent delete = %d, want 1", got)
	}
	f.fx.Exec(t, `DELETE FROM issue WHERE id = $1`, f.parentID)
	if got := f.count(t, `SELECT count(*) FROM issue_child_attention WHERE parent_id = $1 OR child_id = $2`, f.parentID, remainingChildID); got != 0 {
		t.Fatalf("parent deletion retained stale signals: %d", got)
	}
}

func TestChildAttentionConcurrentWorkersDeliverOnce(t *testing.T) {
	f := newChildAttentionFixture(t)
	f.child(t, "blocked")
	services := []*TaskService{
		NewTaskService(db.New(f.pool), f.pool, nil, nil),
		NewTaskService(db.New(f.pool), f.pool, nil, nil),
	}
	results := make(chan int, len(services))
	errs := make(chan error, len(services))
	var wg sync.WaitGroup
	for _, svc := range services {
		wg.Add(1)
		go func(svc *TaskService) {
			defer wg.Done()
			queued, err := svc.RecoverChildAttention(context.Background(), 100)
			results <- queued
			errs <- err
		}(svc)
	}
	wg.Wait()
	close(results)
	close(errs)
	total := 0
	for queued := range results {
		total += queued
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if total != 1 || f.commentCount(t) != 1 || f.taskCount(t) != 1 || f.signalCount(t) != 0 {
		t.Fatalf("concurrent delivery total=%d comments=%d tasks=%d signals=%d; want 1/1/1/0", total, f.commentCount(t), f.taskCount(t), f.signalCount(t))
	}
}
