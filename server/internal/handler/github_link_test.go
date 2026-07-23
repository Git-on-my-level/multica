package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestParseCanonicalGitHubPRURL(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		owner  string
		repo   string
		number int32
		ok     bool
	}{
		{"canonical", "https://github.com/acme/widget/pull/42", "acme", "widget", 42, true},
		{"trailing_slash", "https://github.com/acme/widget/pull/42/", "acme", "widget", 42, true},
		{"with_query_fragment", "https://github.com/acme/widget/pull/7?diff=unified#discussion", "acme", "widget", 7, true},
		{"http_accepted", "http://github.com/acme/widget/pull/1", "acme", "widget", 1, true},
		{"whitespace_trimmed", "  https://github.com/acme/widget/pull/9  ", "acme", "widget", 9, true},
		{"enterprise_host_rejected", "https://github.example.com/acme/widget/pull/1", "", "", 0, false},
		{"non_pull_path_rejected", "https://github.com/acme/widget/issues/1", "", "", 0, false},
		{"files_suffix_rejected", "https://github.com/acme/widget/pull/1/files", "", "", 0, false},
		{"non_numeric_rejected", "https://github.com/acme/widget/pull/abc", "", "", 0, false},
		{"zero_rejected", "https://github.com/acme/widget/pull/0", "", "", 0, false},
		{"not_a_url", "acme/widget#1", "", "", 0, false},
		{"empty", "", "", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo, number, ok := parseCanonicalGitHubPRURL(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && (owner != tc.owner || repo != tc.repo || number != tc.number) {
				t.Errorf("got owner=%q repo=%q number=%d, want %q/%q/%d", owner, repo, number, tc.owner, tc.repo, tc.number)
			}
		})
	}
}

// seedManualLinkInstallation inserts a github_installation row bound to the
// workspace. Tests share one installation id per case; cleanup is
// workspace-scoped.
func seedManualLinkInstallation(t *testing.T, wsID string, installationID int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := testHandler.Queries.CreateGitHubInstallation(ctx, db.CreateGitHubInstallationParams{
		WorkspaceID:    parseUUID(wsID),
		InstallationID: installationID,
		AccountLogin:   "manual-link-acct",
		AccountType:    "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM github_installation WHERE workspace_id = $1`, wsID)
	})
}

// seedMirroredPR inserts a github_pull_request row mirrored in the workspace
// and returns its UUID. The PR is intentionally bare (no checks, no stats) —
// only state + identity matter for the link/unlink/close-gate paths.
func seedMirroredPR(t *testing.T, wsID string, installationID int64, owner, repo string, number int32, state string) string {
	t.Helper()
	ctx := context.Background()
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	params := db.UpsertGitHubPullRequestParams{
		WorkspaceID:    parseUUID(wsID),
		InstallationID: installationID,
		RepoOwner:      owner,
		RepoName:       repo,
		PrNumber:       number,
		Title:          "Mirrored PR " + repo + "#" + strconv.Itoa(int(number)),
		State:          state,
		HtmlUrl:        "https://github.com/" + owner + "/" + repo + "/pull/" + strconv.Itoa(int(number)),
		PrCreatedAt:    now,
		PrUpdatedAt:    now,
		HeadSha:        "deadbeef",
		Additions:      1,
		Deletions:      1,
		ChangedFiles:   1,
	}
	if state == "merged" {
		params.MergedAt = now
		params.ClosedAt = now
	}
	pr, err := testHandler.Queries.UpsertGitHubPullRequest(ctx, params)
	if err != nil {
		t.Fatalf("UpsertGitHubPullRequest: %v", err)
	}
	id := uuidToString(pr.ID)
	t.Cleanup(func() {
		// The link row + PR row are workspace-scoped; clear them so a re-run
		// of the suite doesn't collide on the unique
		// (workspace, owner, repo, number) constraint.
		testPool.Exec(context.Background(), `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, pr.ID)
		testPool.Exec(context.Background(), `DELETE FROM github_pull_request WHERE id = $1`, pr.ID)
	})
	return id
}

var manualLinkIssueSeq uint64

// createManualLinkIssue creates an issue in the test workspace with the given
// status and returns its UUID. The title carries the test name + a sequence
// number so multiple issues per test (and across the shared workspace) never
// trip the active-duplicate guard. Cleanup removes the issue (cascading links).
func createManualLinkIssue(t *testing.T, status string) string {
	t.Helper()
	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "Manual link " + t.Name() + " #" + strconv.FormatUint(atomic.AddUint64(&manualLinkIssueSeq, 1), 10),
		"status": status,
	})
	testHandler.CreateIssue(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateIssue: %d %s", w.Code, w.Body.String())
	}
	var created IssueResponse
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decode issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_pull_request WHERE issue_id = $1`, created.ID)
		testPool.Exec(context.Background(), `DELETE FROM activity_log WHERE issue_id = $1`, created.ID)
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, created.ID)
	})
	return created.ID
}

func linkIssuePR(t *testing.T, issueID, prURL string, closeIntent bool) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"url": prURL, "close_intent": closeIntent}
	req := withURLParam(newRequest("POST", "/api/issues/"+issueID+"/pull-requests/link", body), "id", issueID)
	w := httptest.NewRecorder()
	testHandler.LinkPullRequestToIssue(w, req)
	return w
}

func unlinkIssuePR(t *testing.T, issueID, prURL string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"url": prURL}
	req := withURLParam(newRequest("POST", "/api/issues/"+issueID+"/pull-requests/unlink", body), "id", issueID)
	w := httptest.NewRecorder()
	testHandler.UnlinkPullRequestFromIssue(w, req)
	return w
}

func issueStatus(t *testing.T, issueID string) string {
	t.Helper()
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	return issue.Status
}

func countLinks(t *testing.T, issueID string) int {
	t.Helper()
	linked, err := testHandler.Queries.ListPullRequestsByIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatalf("ListPullRequestsByIssue: %v", err)
	}
	return len(linked)
}

func TestLinkPullRequest_RejectsMalformedURL(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issueID := createManualLinkIssue(t, "in_progress")
	for _, bad := range []string{
		"https://gitlab.com/acme/widget/pull/1",
		"https://github.com/acme/widget/issues/1",
		"not a url",
		"https://github.com/acme/widget/pull/0",
	} {
		w := linkIssuePR(t, issueID, bad, false)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("url %q: expected 400, got %d %s", bad, w.Code, w.Body.String())
		}
	}
}

func TestLinkPullRequest_RequiresUrl(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issueID := createManualLinkIssue(t, "in_progress")
	req := withURLParam(newRequest("POST", "/api/issues/"+issueID+"/pull-requests/link", map[string]any{}), "id", issueID)
	w := httptest.NewRecorder()
	testHandler.LinkPullRequestToIssue(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing url, got %d", w.Code)
	}
}

func TestLinkPullRequest_NotMirroredReturns404(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issueID := createManualLinkIssue(t, "in_progress")
	w := linkIssuePR(t, issueID, "https://github.com/acme/widget/pull/9999", false)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unmirrored PR, got %d %s", w.Code, w.Body.String())
	}
}

func TestLinkPullRequest_DeniesCrossWorkspacePR(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	// A second workspace mirrors the PR; the issue lives in the primary
	// workspace. The link must resolve nothing because GetGitHubPullRequest is
	// scoped to the issue's workspace.
	var otherWS string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, description, issue_prefix)
		VALUES ('Manual link other', 'manual-link-other', '', 'OTH')
		RETURNING id
	`).Scan(&otherWS); err != nil {
		t.Fatalf("create other workspace: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM workspace WHERE id = $1`, otherWS) })

	const instID int64 = 77889900
	seedManualLinkInstallation(t, otherWS, instID)
	seedMirroredPR(t, otherWS, instID, "acme", "xws", 5, "open")

	issueID := createManualLinkIssue(t, "in_progress")
	w := linkIssuePR(t, issueID, "https://github.com/acme/xws/pull/5", false)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-workspace link: expected 404, got %d %s", w.Code, w.Body.String())
	}
	if got := countLinks(t, issueID); got != 0 {
		t.Fatalf("cross-workspace link leaked %d link(s)", got)
	}
}

func TestLinkPullRequest_InstallationNotConnected(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	// Mirror a PR under an installation id that is NOT bound to the workspace.
	// The row outlives any binding (no FK), so the explicit connected-install
	// gate is what rejects the link.
	const orphanInst int64 = 42424242
	issueID := createManualLinkIssue(t, "in_progress")
	seedMirroredPR(t, testWorkspaceID, orphanInst, "acme", "orphan", 3, "open")

	w := linkIssuePR(t, issueID, "https://github.com/acme/orphan/pull/3", false)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for disconnected installation, got %d %s", w.Code, w.Body.String())
	}
}

func TestLinkPullRequest_LinksAndIsIdempotent(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const instID int64 = 10010010
	seedManualLinkInstallation(t, testWorkspaceID, instID)
	seedMirroredPR(t, testWorkspaceID, instID, "acme", "widget", 11, "open")
	issueID := createManualLinkIssue(t, "in_progress")

	url := "https://github.com/acme/widget/pull/11"
	for i := range 2 {
		w := linkIssuePR(t, issueID, url, false)
		if w.Code != http.StatusOK {
			t.Fatalf("link attempt %d: expected 200, got %d %s", i, w.Code, w.Body.String())
		}
	}
	if got := countLinks(t, issueID); got != 1 {
		t.Fatalf("expected 1 link after idempotent double-link, got %d", got)
	}
	// A link without close intent to an open PR must not advance the issue.
	if got := issueStatus(t, issueID); got != "in_progress" {
		t.Fatalf("expected issue to stay in_progress, got %q", got)
	}
}

func TestLinkPullRequest_CloseIntentMergedAdvancesToDone(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const instID int64 = 20020020
	seedManualLinkInstallation(t, testWorkspaceID, instID)
	seedMirroredPR(t, testWorkspaceID, instID, "acme", "merged-repo", 21, "merged")
	issueID := createManualLinkIssue(t, "in_progress")

	w := linkIssuePR(t, issueID, "https://github.com/acme/merged-repo/pull/21", true)
	if w.Code != http.StatusOK {
		t.Fatalf("link: expected 200, got %d %s", w.Code, w.Body.String())
	}
	if got := issueStatus(t, issueID); got != "done" {
		t.Fatalf("expected issue advanced to done, got %q", got)
	}
}

func TestLinkPullRequest_CloseIntentWithoutMergeDoesNotAdvance(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const instID int64 = 20020021
	seedManualLinkInstallation(t, testWorkspaceID, instID)
	seedMirroredPR(t, testWorkspaceID, instID, "acme", "closed-repo", 22, "closed")
	issueID := createManualLinkIssue(t, "in_progress")

	w := linkIssuePR(t, issueID, "https://github.com/acme/closed-repo/pull/22", true)
	if w.Code != http.StatusOK {
		t.Fatalf("link: expected 200, got %d %s", w.Code, w.Body.String())
	}
	if got := issueStatus(t, issueID); got != "in_progress" {
		t.Fatalf("closed-unmerged must not advance issue, got %q", got)
	}
}

func TestLinkPullRequest_CloseIntentFalseOnMergedDoesNotAdvance(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const instID int64 = 20020022
	seedManualLinkInstallation(t, testWorkspaceID, instID)
	seedMirroredPR(t, testWorkspaceID, instID, "acme", "nointent-repo", 23, "merged")
	issueID := createManualLinkIssue(t, "in_progress")

	w := linkIssuePR(t, issueID, "https://github.com/acme/nointent-repo/pull/23", false)
	if w.Code != http.StatusOK {
		t.Fatalf("link: expected 200, got %d %s", w.Code, w.Body.String())
	}
	if got := issueStatus(t, issueID); got != "in_progress" {
		t.Fatalf("close_intent=false must not advance issue, got %q", got)
	}
}

func TestLinkPullRequest_OpenSiblingBlocksAdvance(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const instID int64 = 20020023
	seedManualLinkInstallation(t, testWorkspaceID, instID)
	// Two PRs in distinct repos so the (workspace, owner, repo, number)
	// uniqueness leaves room for both.
	merged := seedMirroredPR(t, testWorkspaceID, instID, "acme", "sib-merged", 31, "merged")
	open := seedMirroredPR(t, testWorkspaceID, instID, "acme", "sib-open", 32, "open")
	issueID := createManualLinkIssue(t, "in_progress")
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM github_pull_request WHERE id IN ($1, $2)`, merged, open)
	})

	// Link the still-open sibling FIRST, so the close gate sees an in-flight
	// PR when the merged one is linked with close intent.
	if w := linkIssuePR(t, issueID, "https://github.com/acme/sib-open/pull/32", true); w.Code != http.StatusOK {
		t.Fatalf("link open sibling: %d %s", w.Code, w.Body.String())
	}
	// Now link the merged PR with close intent: the gate must see open_count=1
	// and refuse to advance.
	if w := linkIssuePR(t, issueID, "https://github.com/acme/sib-merged/pull/31", true); w.Code != http.StatusOK {
		t.Fatalf("link merged: %d %s", w.Code, w.Body.String())
	}
	if got := issueStatus(t, issueID); got != "in_progress" {
		t.Fatalf("open sibling must block advance, got %q", got)
	}
}

func TestLinkPullRequest_PreservesCancelled(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const instID int64 = 20020024
	seedManualLinkInstallation(t, testWorkspaceID, instID)
	seedMirroredPR(t, testWorkspaceID, instID, "acme", "cancel-repo", 41, "merged")
	issueID := createManualLinkIssue(t, "cancelled")

	w := linkIssuePR(t, issueID, "https://github.com/acme/cancel-repo/pull/41", true)
	if w.Code != http.StatusOK {
		t.Fatalf("link: expected 200, got %d %s", w.Code, w.Body.String())
	}
	if got := issueStatus(t, issueID); got != "cancelled" {
		t.Fatalf("cancelled issue must not advance, got %q", got)
	}
}

func TestUnlinkPullRequest_RemovesLinkAndNeverReopens(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const instID int64 = 30030030
	seedManualLinkInstallation(t, testWorkspaceID, instID)
	seedMirroredPR(t, testWorkspaceID, instID, "acme", "unlink-repo", 51, "open")
	issueID := createManualLinkIssue(t, "in_progress")
	url := "https://github.com/acme/unlink-repo/pull/51"

	if w := linkIssuePR(t, issueID, url, false); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body.String())
	}
	if got := countLinks(t, issueID); got != 1 {
		t.Fatalf("expected 1 link, got %d", got)
	}

	if w := unlinkIssuePR(t, issueID, url); w.Code != http.StatusOK {
		t.Fatalf("unlink: expected 200, got %d %s", w.Code, w.Body.String())
	}
	if got := countLinks(t, issueID); got != 0 {
		t.Fatalf("expected 0 links after unlink, got %d", got)
	}

	// Unlinking a done issue must not reopen it. Link a merged close-intent PR
	// (advances to done), then unlink — status stays done.
	doneIssue := createManualLinkIssue(t, "in_progress")
	seedMirroredPR(t, testWorkspaceID, instID, "acme", "reopen-repo", 52, "merged")
	doneURL := "https://github.com/acme/reopen-repo/pull/52"
	if w := linkIssuePR(t, doneIssue, doneURL, true); w.Code != http.StatusOK {
		t.Fatalf("link for done: %d %s", w.Code, w.Body.String())
	}
	if got := issueStatus(t, doneIssue); got != "done" {
		t.Fatalf("expected done before unlink, got %q", got)
	}
	if w := unlinkIssuePR(t, doneIssue, doneURL); w.Code != http.StatusOK {
		t.Fatalf("unlink done: %d %s", w.Code, w.Body.String())
	}
	if got := issueStatus(t, doneIssue); got != "done" {
		t.Fatalf("unlink must never reopen a done issue, got %q", got)
	}
}

func TestUnlinkPullRequest_NotMirroredReturns404(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issueID := createManualLinkIssue(t, "in_progress")
	w := unlinkIssuePR(t, issueID, "https://github.com/acme/never-mirrored/pull/99")
	if w.Code != http.StatusNotFound {
		t.Fatalf("unlink unmirrored: expected 404, got %d %s", w.Code, w.Body.String())
	}
}

// firePullRequestWebhookRaw delivers a signed pull_request webhook with full
// control over action/title/body/branch. Unlike the multi-PR suite's
// firePullRequestWebhook (which titles the PR "Fix <id>", a closing keyword),
// this one lets a test place the issue identifier in the title WITHOUT a
// closing keyword — the exact shape that exposes the manual-link clobber. The
// path is exercised end-to-end through HandleGitHubWebhook (HMAC + installation
// lookup + mirror + auto-link), not just the handler's internals.
func firePullRequestWebhookRaw(t *testing.T, secret string, instID int64, owner, repo string, number int32, action, state string, merged bool, title, body, branch string) {
	t.Helper()
	payload := map[string]any{
		"action": action,
		"pull_request": map[string]any{
			"number":     number,
			"html_url":   "https://github.com/" + owner + "/" + repo + "/pull/" + strconv.Itoa(int(number)),
			"title":      title,
			"body":       body,
			"state":      state,
			"draft":      false,
			"merged":     merged,
			"created_at": "2026-07-23T00:00:00Z",
			"updated_at": "2026-07-23T00:00:00Z",
			"head":       map[string]any{"ref": branch, "sha": "cafebabe"},
			"user":       map[string]any{"login": "octocat", "avatar_url": ""},
		},
		"repository":   map[string]any{"name": repo, "owner": map[string]any{"login": owner}},
		"installation": map[string]any{"id": instID},
	}
	raw, _ := json.Marshal(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	req := httptest.NewRequest("POST", "/api/webhooks/github", bytes.NewReader(raw))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	testHandler.HandleGitHubWebhook(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("webhook %s: expected 202, got %d %s", action, rec.Code, rec.Body.String())
	}
}

// TestWebhook_DoesNotClobberManualLink is the regression guard for the
// manual-link / webhook coexistence bug: a manually-linked PR with
// close_intent=true must survive routine webhook activity (a synchronize push
// whose title carries the issue identifier but no closing keyword). Without
// the member-link preservation in mirrorPullRequestForWorkspace, that push
// would overwrite close_intent true->false and silently kill "mark done when
// merged"; a bare body mention would additionally flip reference_only and hide
// the PR from the list.
func TestWebhook_DoesNotClobberManualLink(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	secret := "manual-coexist-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	issueID := createManualLinkIssue(t, "in_progress")
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(issueID))
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	const instID int64 = 60060060
	seedManualLinkInstallation(t, testWorkspaceID, instID)
	// Mirror the PR first (manual link requires a mirrored row), then link it
	// by hand with close intent. The PR title intentionally references the
	// issue identifier WITHOUT a closing keyword, so the webhook's own auto-link
	// would compute closeIntent=false — exactly the clobber we must prevent.
	seedMirroredPR(t, testWorkspaceID, instID, "acme", "coexist", 71, "open")
	url := "https://github.com/acme/coexist/pull/71"
	if w := linkIssuePR(t, issueID, url, true); w.Code != http.StatusOK {
		t.Fatalf("manual link: %d %s", w.Code, w.Body.String())
	}

	// A routine push: title carries the identifier, no closing keyword. Build
	// the identifier from the workspace prefix + issue number (db.Issue has no
	// Identifier field).
	var prefix string
	if err := testPool.QueryRow(ctx, `SELECT issue_prefix FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&prefix); err != nil {
		t.Fatalf("load workspace prefix: %v", err)
	}
	identifier := fmt.Sprintf("%s-%d", prefix, issue.Number)
	firePullRequestWebhookRaw(t, secret, instID, "acme", "coexist", 71, "synchronize", "open", false,
		identifier+" tweak", "", "feature/x")

	// Resolve the link row via the mirrored PR id and assert the webhook did
	// not regress the member-authored close_intent or flip reference_only.
	pr, err := testHandler.Queries.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{
		WorkspaceID: parseUUID(testWorkspaceID), RepoOwner: "acme", RepoName: "coexist", PrNumber: 71,
	})
	if err != nil {
		t.Fatalf("GetGitHubPullRequest: %v", err)
	}
	link, err := testHandler.Queries.GetIssuePullRequestLink(ctx, db.GetIssuePullRequestLinkParams{
		IssueID: parseUUID(issueID), PullRequestID: pr.ID,
	})
	if err != nil {
		t.Fatalf("GetIssuePullRequestLink after webhook: %v", err)
	}
	if !link.CloseIntent {
		t.Errorf("webhook clobbered manual close_intent: expected true, got false")
	}
	if link.ReferenceOnly {
		t.Errorf("webhook flipped reference_only: expected false, got true (PR would vanish from the list)")
	}
	if got := countLinks(t, issueID); got != 1 {
		t.Errorf("expected PR to remain visible (1 link), got %d", got)
	}
}
