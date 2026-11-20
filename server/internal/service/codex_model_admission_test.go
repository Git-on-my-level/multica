package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/featureflags"
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/featureflag"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

const codexUnsupportedSolMessage = "The 'gpt-6.2-sol' model is not supported when using Codex with a ChatGPT account."
const codexUnsupportedSol = `{"type":"error","status":400,"error":{"type":"invalid_request_error","message":"The 'gpt-6.2-sol' model is not supported when using Codex with a ChatGPT account.","code":"model_not_found"}}`

func TestCodexRejectedModel(t *testing.T) {
	for _, tc := range []struct {
		name, message, want string
	}{
		{"live provider payload", codexUnsupportedSol, "gpt-6.2-sol"},
		{"legacy daemon wrapper", "API error 400: " + codexUnsupportedSol, "gpt-6.2-sol"},
		{"Codex transport wrapper", "Error running turn: unexpected status 400 Bad Request, body: " + codexUnsupportedSol + ", url: https://chatgpt.com/backend-api/codex/responses", "gpt-6.2-sol"},
		{"adapter extracts only message", codexUnsupportedSolMessage, "gpt-6.2-sol"},
		{"ordinary message about rejection", "The log read: " + codexUnsupportedSolMessage, ""},
		{"non-400 wrapper", "API error 503: " + codexUnsupportedSol, ""},
		{"other 400", strings.Replace(codexUnsupportedSol, "model_not_found", "invalid_model", 1), ""},
		{"generic model not found", strings.Replace(codexUnsupportedSol, " is not supported when using Codex with a ChatGPT account.", " does not exist.", 1), ""},
		{"transient status", strings.Replace(codexUnsupportedSol, "400", "503", 1), ""},
		{"text only", "model_not_found", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexRejectedModel(tc.message); got != tc.want {
				t.Fatalf("codexRejectedModel = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCodexModelAdmissionDispatch(t *testing.T) {
	ctx := context.Background()
	pool := sharedTestPool(t)
	base := dbfx.New(pool, "", "")
	suffix := fmt.Sprint(time.Now().UnixNano())
	userID := base.User(t, "Codex admission", "codex-admission-"+suffix+"@multica.test")
	workspaceID := base.Workspace(t, "Codex admission", "codex-admission-"+suffix)
	fx := dbfx.New(pool, workspaceID, userID)
	runtimeID := fx.Runtime(t, "Codex", dbfx.Cols{
		"provider": "codex",
		"metadata": dbfx.Raw(`'{"version":"codex-cli 0.159.0"}'::jsonb`),
	})
	sol := fx.Agent(t, "Sol", runtimeID, dbfx.Cols{"model": "gpt-6.2-sol", "max_concurrent_tasks": 2})
	luna := fx.Agent(t, "Luna", runtimeID, dbfx.Cols{"model": "gpt-6-luna"})
	service := NewTaskService(db.New(pool), pool, nil, events.New())
	queue := func(agent, title string) pgtype.UUID {
		issueID := fx.Issue(t, title)
		return util.MustParseUUID(fx.Task(t, agent, dbfx.Cols{"runtime_id": runtimeID, "issue_id": issueID}))
	}
	claim := func(agent string, want pgtype.UUID) {
		t.Helper()
		task, err := service.ClaimTask(ctx, util.MustParseUUID(agent))
		if err != nil || (!want.Valid && task != nil) || (want.Valid && (task == nil || task.ID != want)) {
			t.Fatalf("claim %s = %v, %v, want %s", agent, task, err, util.UUIDToString(want))
		}
	}
	start := func(id pgtype.UUID) {
		t.Helper()
		if _, err := service.StartTask(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	fail := func(id pgtype.UUID, message string, reason taskfailure.Reason) {
		t.Helper()
		if _, err := service.FailTask(ctx, id, message, "", "", "", reason.String(), false, "", ""); err != nil {
			t.Fatal(err)
		}
	}

	first := queue(sol, "Ordinary invalid request")
	claim(sol, first)
	start(first)
	fail(first, "API error 400: bad image", taskfailure.ReasonAPIInvalidRequest)
	transient := queue(sol, "Transient rate limit")
	claim(sol, transient)
	start(transient)
	fail(transient, "API error 429: too many requests", taskfailure.ReasonAgentProviderCapacityOrRateLimit)

	t.Run("unparseable wrapped rejection", func(t *testing.T) {
		id := queue(sol, "Unparseable Codex rejection")
		claimed, err := service.ClaimTask(ctx, util.MustParseUUID(sol))
		if err != nil || claimed == nil || claimed.ID != id {
			t.Fatalf("claim unparseable rejection fixture: %v, %v", claimed, err)
		}
		if _, err := service.StartTask(ctx, id); err != nil {
			t.Fatal(err)
		}

		path := filepath.Join(t.TempDir(), "failure.log")
		logFile, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = logFile.Close() })
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(logFile, nil)))
		t.Cleanup(func() { slog.SetDefault(previous) })

		const damaged = `API error 400: {"code":"model_not_found","message":"` + codexUnsupportedSolMessage
		failed, err := service.FailTask(ctx, id, damaged, "", "", "", taskfailure.ReasonAPIInvalidRequest.String(), false, "", "")
		if err != nil || failed == nil || failed.Status != "failed" ||
			failed.FailureReason.String != taskfailure.ReasonAPIInvalidRequest.String() || failed.Error.String != damaged {
			t.Fatalf("expected ordinary failure with its existing diagnosis: %v, %v", failed, err)
		}
		agent, err := service.Queries.GetAgent(ctx, util.MustParseUUID(sol))
		if err != nil || len(agent.CodexModelAdmissionRejection) != 0 {
			t.Fatalf("unparseable failure recorded a model rejection: %s, %v", agent.CodexModelAdmissionRejection, err)
		}
		logs, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(logs), "\n") {
			if strings.Contains(line, "level=WARN") && strings.Contains(line, "unrecognized Codex model rejection format") &&
				strings.Contains(line, "task_id="+util.UUIDToString(id)) && strings.Contains(line, "agent_id="+sol) {
				return
			}
		}
		t.Fatalf("missing warning for the unparseable failure: %s", logs)
	})

	rejected := queue(sol, "Unsupported configured model")
	claim(sol, rejected)
	start(rejected)
	fail(rejected, "API error 400: "+codexUnsupportedSol, taskfailure.ReasonAPIInvalidRequest)

	waiting := queue(sol, "Wait for model admission")
	peer := queue(luna, "Supported model on the same runtime")
	runtimeUUID := util.MustParseUUID(runtimeID)
	for poll := range 3 {
		claim(sol, pgtype.UUID{})
		got, _, err := service.ClaimTasksForRuntimes(ctx, []pgtype.UUID{runtimeUUID}, 4)
		if err != nil || (poll == 0 && (len(got) != 1 || got[0].ID != peer)) || (poll > 0 && len(got) != 0) {
			t.Fatalf("batch poll %d = %v, %v", poll, got, err)
		}
		held, err := service.Queries.GetAgentTask(ctx, waiting)
		if err != nil || held.Status != "queued" || held.DispatchedAt.Valid || held.StartedAt.Valid {
			t.Fatalf("queued task was dispatched while configuration was rejected: %v, %v", held, err)
		}
	}

	fx.Exec(t, `UPDATE agent_runtime SET metadata = '{"version":"codex-cli 0.168.0"}' WHERE id = $1`, runtimeID)
	claim(sol, waiting)
	start(waiting)
	// The replacement configuration can be tested. If it also fails, it is
	// quarantined independently until a verified repair changes the revision.
	fail(waiting, "Error running turn: unexpected status 400 Bad Request, body: "+codexUnsupportedSol, taskfailure.ReasonAgentModelNotFoundOrUnavailable)
	repaired := queue(sol, "Same-version catalog repair")
	claim(sol, pgtype.UUID{})
	fx.Exec(t, `UPDATE agent SET runtime_config = '{"codex_model_admission_revision":"catalog-repair-1"}' WHERE id = $1`, sol)
	claim(sol, repaired)
	start(repaired)

	// A late error from before a version change cannot quarantine the new one.
	fx.Exec(t, `UPDATE agent_runtime SET metadata = '{"version":"codex-cli 0.169.0"}' WHERE id = $1`, runtimeID)
	fail(repaired, codexUnsupportedSol, taskfailure.ReasonAPIInvalidRequest)
	newVersion := queue(sol, "New version after a late failure")
	claim(sol, newVersion)
	start(newVersion)
	fail(newVersion, codexUnsupportedSol, taskfailure.ReasonAPIInvalidRequest)
	fx.Exec(t, `UPDATE agent SET model = 'gpt-6-luna' WHERE id = $1`, sol)
	newModel := queue(sol, "Supported replacement model")
	claim(sol, newModel)
	start(newModel)
	if _, err := service.CompleteTask(ctx, newModel, []byte("{}"), "", "", "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	claim(sol, queue(sol, "Dispatch continues after successful admission"))

	// Restoring the rejected pin does not clear its hold.
	fx.Exec(t, `UPDATE agent SET model = 'gpt-6.2-sol' WHERE id = $1`, sol)
	rollback := queue(sol, "Breaker disabled")
	claim(sol, pgtype.UUID{})
	// The flag bypasses the hold without changing the configured model.
	flags := featureflag.NewStaticProvider()
	flags.Set(featureflags.CodexModelAdmission, featureflag.Rule{Default: false})
	service.FeatureFlags = featureflag.NewService(flags)
	claim(sol, rollback)
}
