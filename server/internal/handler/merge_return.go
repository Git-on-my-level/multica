package handler

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type mergeReturnPreparationKey struct{}

// A distinct endpoint makes old servers reject preparation before any write.
// Silently ignoring an unknown JSON field on ordinary UpdateIssue is unsafe.
func (h *Handler) PrepareMergeReturn(w http.ResponseWriter, r *http.Request) {
	h.UpdateIssue(w, r.WithContext(context.WithValue(r.Context(), mergeReturnPreparationKey{}, true)))
}

func (h *Handler) prepareMergeReturn(ctx context.Context, workspace pgtype.UUID, status string, params db.UpdateIssueParams, custody json.RawMessage) (db.Issue, error) {
	var out db.Issue
	tx, err := h.beginWakeupWrite(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	if err = assertIssueStatusStillActive(ctx, q, workspace, status); err != nil {
		return out, err
	}
	i, err := q.LockWakeupIssue(ctx, params.ID)
	if err != nil {
		return out, err
	}
	if err = service.ValidateMergeReturnPreparation(ctx, tx, q, i, params, custody); err != nil {
		return out, err
	}
	out, err = q.UpdateIssue(ctx, params)
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}
