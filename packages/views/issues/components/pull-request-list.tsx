"use client";

import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  CheckCircle2,
  CircleDashed,
  GitMerge,
  GitPullRequest,
  GitPullRequestArrow,
  GitPullRequestClosed,
  GitPullRequestDraft,
  Loader2,
  TriangleAlert,
  Unlink,
  XCircle,
} from "lucide-react";
import { issueKeys } from "@multica/core/issues/queries";
import {
  issuePullRequestsOptions,
  derivePullRequestStatusKind,
  derivePullRequestProgressSegments,
  shouldShowPullRequestStats,
  type PullRequestStatusKind,
  type PullRequestProgressSegment,
} from "@multica/core/github";
import { api } from "@multica/core/api";
import type {
  GitHubPullRequest,
  GitHubPullRequestChecksConclusion,
  GitHubPullRequestState,
} from "@multica/core/types";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { Button } from "@multica/ui/components/ui/button";
import { Checkbox } from "@multica/ui/components/ui/checkbox";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";

type IssuesT = ReturnType<typeof useT<"issues">>["t"];

// Keep the existing sidebar density: show the first 3 PR rows inline, then
// collapse the rest once the section reaches 4 rows.
const PR_LIMIT_BEFORE_COLLAPSE = 4;

// Canonical GitHub PR URL. The server canonicalises and additionally requires
// the PR to already be mirrored in this workspace; this is only a client-side
// shape guard so the submit button stays disabled until the input looks like a
// real PR link, and a clear inline error replaces a round-trip 400.
const GITHUB_PR_URL_RE = /^https:\/\/github\.com\/[^/]+\/[^/]+\/pull\/\d+\/?(\?.*)?$/i;

const STATE_ICON: Record<
  GitHubPullRequestState,
  { icon: React.ComponentType<{ className?: string }>; className: string }
> = {
  open: { icon: GitPullRequestArrow, className: "text-emerald-600 dark:text-emerald-400" },
  draft: { icon: GitPullRequestDraft, className: "text-muted-foreground" },
  merged: { icon: GitMerge, className: "text-violet-600 dark:text-violet-400" },
  closed: { icon: GitPullRequestClosed, className: "text-rose-600 dark:text-rose-400" },
};

const CHECKS_ICON: Record<
  GitHubPullRequestChecksConclusion,
  { icon: React.ComponentType<{ className?: string }>; className: string }
> = {
  passed: { icon: CheckCircle2, className: "text-emerald-600 dark:text-emerald-400" },
  failed: { icon: XCircle, className: "text-rose-600 dark:text-rose-400" },
  pending: { icon: CircleDashed, className: "text-amber-600 dark:text-amber-400" },
};

export function PullRequestList({ issueId, wsId }: { issueId: string; wsId: string }) {
  const { t } = useT("issues");
  const qc = useQueryClient();
  const { data, isLoading } = useQuery(issuePullRequestsOptions(issueId));
  const prs = data?.pull_requests ?? [];

  // Link dialog state. The dialog stays open on error so the user can fix the
  // URL or read the server message (e.g. "PR not mirrored in this workspace").
  const [linkOpen, setLinkOpen] = useState(false);
  const [url, setUrl] = useState("");
  const [closeIntent, setCloseIntent] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Unlink confirm dialog state. One dialog at the list level rather than one
  // per row — the target PR is captured here when a row's unlink button fires.
  const [unlinkTarget, setUnlinkTarget] = useState<GitHubPullRequest | null>(null);
  const [unlinkError, setUnlinkError] = useState<string | null>(null);

  const resetLinkDialog = () => {
    setUrl("");
    setCloseIntent(false);
    setError(null);
  };

  const linkMutation = useMutation({
    mutationFn: (vars: { url: string; closeIntent: boolean }) =>
      api.linkIssuePullRequest(issueId, vars.url, vars.closeIntent),
    onSuccess: (_data, vars) => {
      // Realtime also invalidates this tree on pull_request:linked, but we
      // invalidate directly so the UI converges even without an open socket.
      qc.invalidateQueries({ queryKey: ["github", "pull-requests"] });
      // Close intent on an already-merged PR can advance the issue to done in
      // the same response; refresh issue caches when the socket is absent.
      if (vars.closeIntent) {
        qc.invalidateQueries({ queryKey: issueKeys.detail(wsId, issueId) });
        qc.invalidateQueries({ queryKey: issueKeys.list(wsId) });
        qc.invalidateQueries({ queryKey: issueKeys.myAll(wsId) });
      }
      setLinkOpen(false);
      resetLinkDialog();
    },
    onError: (e: unknown) => setError(e instanceof Error ? e.message : String(e)),
  });

  const unlinkMutation = useMutation({
    mutationFn: (prUrl: string) => api.unlinkIssuePullRequest(issueId, prUrl),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["github", "pull-requests"] });
      setUnlinkTarget(null);
      setUnlinkError(null);
    },
    onError: (e: unknown) => setUnlinkError(e instanceof Error ? e.message : String(e)),
  });

  const trimmedUrl = url.trim();
  const urlValid = GITHUB_PR_URL_RE.test(trimmedUrl);
  const showUrlError = trimmedUrl.length > 0 && !urlValid;

  const handleSubmitLink = () => {
    if (!urlValid || linkMutation.isPending) return;
    linkMutation.mutate({ url: trimmedUrl, closeIntent });
  };

  return (
    <div className="space-y-1">
      <div className="flex items-center justify-end -mx-2 px-2">
        <Button
          type="button"
          variant="link"
          size="xs"
          className="h-5 px-1 text-[11px] text-muted-foreground"
          onClick={() => {
            resetLinkDialog();
            setLinkOpen(true);
          }}
        >
          <GitPullRequestArrow className="h-3 w-3" />
          {t(($) => $.detail.pull_request_link_action)}
        </Button>
      </div>

      {isLoading ? (
        <p className="text-xs text-muted-foreground px-2">{t(($) => $.detail.pull_requests_loading)}</p>
      ) : prs.length === 0 ? (
        <p className="text-xs text-muted-foreground px-2">
          {t(($) => $.detail.pull_requests_empty)}
        </p>
      ) : (
        <PullRequestRows
          prs={prs}
          onUnlink={(pr) => {
            setUnlinkError(null);
            setUnlinkTarget(pr);
          }}
        />
      )}

      <Dialog
        open={linkOpen}
        onOpenChange={(open) => {
          setLinkOpen(open);
          if (!open && !linkMutation.isPending) resetLinkDialog();
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t(($) => $.detail.pull_request_link_dialog_title)}</DialogTitle>
          </DialogHeader>
          <div className="space-y-2">
            <Label htmlFor="pr-link-url">{t(($) => $.detail.pull_request_link_url_label)}</Label>
            <Input
              id="pr-link-url"
              value={url}
              onChange={(e) => {
                setUrl(e.target.value);
                setError(null);
              }}
              placeholder={t(($) => $.detail.pull_request_link_url_placeholder)}
              aria-invalid={showUrlError || !!error}
              autoFocus
            />
            {showUrlError ? (
              <p className="text-xs text-destructive">
                {t(($) => $.detail.pull_request_link_url_invalid)}
              </p>
            ) : null}
            {error ? <p className="text-xs text-destructive">{error}</p> : null}
            <Label
              htmlFor="pr-link-close-intent"
              className="font-normal text-muted-foreground"
            >
              <Checkbox
                id="pr-link-close-intent"
                checked={closeIntent}
                onCheckedChange={(checked) => setCloseIntent(checked === true)}
              />
              {t(($) => $.detail.pull_request_link_close_intent)}
            </Label>
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              disabled={linkMutation.isPending}
              onClick={() => setLinkOpen(false)}
            >
              {t(($) => $.detail.pull_request_link_cancel)}
            </Button>
            <Button
              type="button"
              onClick={handleSubmitLink}
              disabled={!urlValid || linkMutation.isPending}
            >
              {linkMutation.isPending ? (
                <Loader2 className="h-3.5 w-3.5 animate-spin" />
              ) : null}
              {t(($) => $.detail.pull_request_link_submit)}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <AlertDialog
        open={unlinkTarget !== null}
        onOpenChange={(open) => {
          if (!open && !unlinkMutation.isPending) setUnlinkTarget(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t(($) => $.detail.pull_request_link_unlink_confirm)}
            </AlertDialogTitle>
          </AlertDialogHeader>
          {unlinkError ? (
            <p className="text-xs text-destructive">{unlinkError}</p>
          ) : null}
          <AlertDialogFooter>
            <AlertDialogCancel disabled={unlinkMutation.isPending}>
              {t(($) => $.detail.pull_request_link_cancel)}
            </AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              disabled={unlinkMutation.isPending || !unlinkTarget}
              onClick={(event) => {
                // AlertDialogAction closes by default. Keep the confirmation open
                // while the mutation runs so a server error remains visible.
                event.preventDefault();
                if (unlinkTarget) unlinkMutation.mutate(unlinkTarget.html_url);
              }}
            >
              {unlinkMutation.isPending ? (
                <Loader2 className="h-3.5 w-3.5 animate-spin" />
              ) : null}
              {t(($) => $.detail.pull_request_link_unlink_confirm_action)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

// The collapsible PR rows. Split out so the list shell (header button,
// loading / empty states, dialogs) stays readable; the collapse toggle state
// lives here next to the rows it controls.
function PullRequestRows({
  prs,
  onUnlink,
}: {
  prs: GitHubPullRequest[];
  onUnlink: (pr: GitHubPullRequest) => void;
}) {
  const { t } = useT("issues");
  const [expanded, setExpanded] = useState(false);

  // Render rule:
  //   - <  PR_LIMIT_BEFORE_COLLAPSE: every PR row is visible.
  //   - >= PR_LIMIT_BEFORE_COLLAPSE: first (LIMIT - 1) rows are visible and
  //     the remainder sits behind a toggle.
  const useCollapse = prs.length >= PR_LIMIT_BEFORE_COLLAPSE;
  const expandedHead = useCollapse ? prs.slice(0, PR_LIMIT_BEFORE_COLLAPSE - 1) : prs;
  const collapsedTail = useCollapse ? prs.slice(PR_LIMIT_BEFORE_COLLAPSE - 1) : [];

  return (
    <>
      {expandedHead.map((pr) => (
        <PullRequestRow key={pr.id} pr={pr} onUnlink={onUnlink} />
      ))}
      {useCollapse ? (
        <div className="space-y-1">
          {expanded
            ? collapsedTail.map((pr) => (
                <PullRequestRow key={pr.id} pr={pr} onUnlink={onUnlink} />
              ))
            : null}
          <button
            type="button"
            onClick={() => setExpanded((v) => !v)}
            className="block w-[calc(100%+1rem)] -mx-2 rounded-md px-2 py-1.5 text-left text-[11px] text-muted-foreground hover:bg-accent/50 hover:text-foreground transition-colors"
          >
            {expanded
              ? t(($) => $.detail.pull_request_card_show_less)
              : t(($) => $.detail.pull_request_card_show_more, { count: collapsedTail.length })}
          </button>
        </div>
      ) : null}
    </>
  );
}

function PullRequestRow({
  pr,
  onUnlink,
}: {
  pr: GitHubPullRequest;
  onUnlink: (pr: GitHubPullRequest) => void;
}) {
  const { t } = useT("issues");
  const cfg = STATE_ICON[pr.state] ?? { icon: GitPullRequest, className: "" };
  const StateIcon = cfg.icon;
  const kind = derivePullRequestStatusKind({
    state: pr.state,
    mergeable_state: pr.mergeable_state,
    checks_failed: pr.checks_failed,
    checks_pending: pr.checks_pending,
    checks_passed: pr.checks_passed,
  });
  const segments = derivePullRequestProgressSegments({
    state: pr.state,
    checks_failed: pr.checks_failed,
    checks_pending: pr.checks_pending,
    checks_passed: pr.checks_passed,
  });
  const showStats = shouldShowPullRequestStats({
    additions: pr.additions,
    deletions: pr.deletions,
    changed_files: pr.changed_files,
  });
  const statusText = useStatusText(kind);
  const draftPrefix = pr.state === "draft";
  const stateLabel = getStateLabel(pr.state, t);

  return (
    <div className="group/row relative">
      <a
        data-testid="pull-request-row"
        href={pr.html_url}
        target="_blank"
        rel="noreferrer noopener"
        className={cn(
          "flex items-start gap-2 rounded-md px-2 py-1.5 pr-7 -mx-2 hover:bg-accent/50 transition-colors group",
          draftPrefix ? "opacity-80" : null,
        )}
      >
        <StateIcon className={cn("h-3.5 w-3.5 mt-0.5 shrink-0", cfg.className)} />
        <div className="min-w-0 flex-1">
          <p className="text-xs font-medium leading-snug truncate group-hover:text-foreground">
            {pr.title}
          </p>
          <p className="text-[11px] text-muted-foreground truncate">
            {pr.repo_owner}/{pr.repo_name}#{pr.number} · {stateLabel}
            {pr.author_login ? ` · @${pr.author_login}` : null}
          </p>
          <PullRequestRowDetails
            pr={pr}
            segments={segments}
            showStats={showStats}
            statusText={
              draftPrefix
                ? t(($) => $.detail.pull_request_card_draft_prefix, { status: statusText })
                : statusText
            }
            statusKind={kind}
          />
        </div>
      </a>
      <button
        type="button"
        aria-label={t(($) => $.detail.pull_request_link_unlink)}
        onClick={() => onUnlink(pr)}
        className="absolute right-0.5 top-1.5 z-10 grid size-5 place-items-center rounded text-muted-foreground opacity-0 transition-opacity hover:text-foreground focus-visible:opacity-100 group-hover/row:opacity-100"
      >
        <Unlink className="h-3 w-3" />
      </button>
    </div>
  );
}

function PullRequestRowDetails({
  pr,
  segments,
  showStats,
  statusText,
  statusKind,
}: {
  pr: GitHubPullRequest;
  segments: PullRequestProgressSegment[] | null;
  showStats: boolean;
  statusText: string;
  statusKind: PullRequestStatusKind;
}) {
  const { t } = useT("issues");
  const checksBadge = getChecksBadge(pr, t);
  const conflictsBadge = getConflictsBadge(pr, t);
  const isTerminal = statusKind === "closed" || statusKind === "merged";
  const showChecksBadge =
    !isTerminal &&
    !!checksBadge &&
    statusKind !== "checks_failed" &&
    statusKind !== "checks_pending" &&
    statusKind !== "checks_passed";
  const showConflictsBadge =
    !isTerminal && !!conflictsBadge && statusKind !== "conflicts" && statusKind !== "ready";

  return (
    <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[11px] text-muted-foreground">
      {showStats ? <PullRequestStats pr={pr} /> : null}
      <PullRequestProgressStrip segments={segments} />
      <span className="truncate">{statusText}</span>
      {showChecksBadge ? <PullRequestBadge badge={checksBadge} /> : null}
      {showConflictsBadge ? <PullRequestBadge badge={conflictsBadge} /> : null}
    </div>
  );
}

function PullRequestStats({ pr }: { pr: GitHubPullRequest }) {
  const { t } = useT("issues");
  return (
    <span className="inline-flex items-center gap-1.5 tabular-nums">
      <span className="text-emerald-600 dark:text-emerald-400">+{pr.additions ?? 0}</span>
      <span className="text-rose-600 dark:text-rose-400">−{pr.deletions ?? 0}</span>
      <span aria-hidden="true">·</span>
      <span>
        {t(($) => $.detail.pull_request_card_files_count, {
          count: pr.changed_files ?? 0,
        })}
      </span>
    </span>
  );
}

function PullRequestProgressStrip({
  segments,
}: {
  segments: PullRequestProgressSegment[] | null;
}) {
  if (!segments) return null;
  return (
    <span className="flex h-1 w-12 shrink-0 overflow-hidden rounded-full bg-muted" aria-hidden="true">
      {segments.map((seg) => (
        <span
          key={seg.kind}
          className={cn(
            "h-full block",
            seg.kind === "failed" && "bg-rose-500 dark:bg-rose-400",
            seg.kind === "pending" && "bg-amber-500 dark:bg-amber-400",
            seg.kind === "passed" && "bg-emerald-500 dark:bg-emerald-400",
          )}
          style={{ width: `${seg.ratio * 100}%` }}
        />
      ))}
    </span>
  );
}

interface PullRequestBadgeConfig {
  icon: React.ComponentType<{ className?: string }>;
  label: string;
  className: string;
}

function PullRequestBadge({ badge }: { badge: PullRequestBadgeConfig }) {
  const Icon = badge.icon;
  return (
    <span className="inline-flex items-center gap-1">
      <Icon className={cn("h-3 w-3", badge.className)} />
      {badge.label}
    </span>
  );
}

function getConflictsBadge(
  pr: GitHubPullRequest,
  t: IssuesT,
): PullRequestBadgeConfig | null {
  const mergeable = pr.mergeable_state ?? null;
  return mergeable === "dirty"
    ? {
        icon: TriangleAlert,
        label: t(($) => $.detail.pull_request_conflicts_dirty),
        className: "text-rose-600 dark:text-rose-400",
      }
    : mergeable === "clean"
      ? {
          icon: CheckCircle2,
          label: t(($) => $.detail.pull_request_conflicts_clean),
          className: "text-emerald-600 dark:text-emerald-400",
        }
      : null;
}

function getChecksBadge(
  pr: GitHubPullRequest,
  t: IssuesT,
): PullRequestBadgeConfig | null {
  const checks = pr.checks_conclusion ?? null;
  return checks && CHECKS_ICON[checks]
    ? {
        icon: CHECKS_ICON[checks].icon,
        className: CHECKS_ICON[checks].className,
        label:
          checks === "passed"
            ? t(($) => $.detail.pull_request_checks_passed)
            : checks === "failed"
              ? t(($) => $.detail.pull_request_checks_failed)
              : t(($) => $.detail.pull_request_checks_pending),
      }
    : null;
}

function getStateLabel(
  state: GitHubPullRequestState,
  t: IssuesT,
): string {
  return state === "open"
    ? t(($) => $.detail.pull_request_state_open)
    : state === "draft"
      ? t(($) => $.detail.pull_request_state_draft)
      : state === "merged"
        ? t(($) => $.detail.pull_request_state_merged)
        : state === "closed"
          ? t(($) => $.detail.pull_request_state_closed)
          : state;
}

function useStatusText(kind: PullRequestStatusKind): string {
  const { t } = useT("issues");
  switch (kind) {
    case "closed":
      return t(($) => $.detail.pull_request_card_status_closed);
    case "merged":
      return t(($) => $.detail.pull_request_card_status_merged);
    case "conflicts":
      return t(($) => $.detail.pull_request_card_status_conflicts);
    case "checks_failed":
      return t(($) => $.detail.pull_request_card_status_checks_failed);
    case "checks_pending":
      return t(($) => $.detail.pull_request_card_status_checks_pending);
    case "checks_passed":
      return t(($) => $.detail.pull_request_card_status_checks_passed);
    case "ready":
      return t(($) => $.detail.pull_request_card_status_ready);
    case "unknown":
      return t(($) => $.detail.pull_request_card_status_unknown);
  }
}
