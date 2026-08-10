package handler

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	workspaceEventDefaultLimit = 100
	workspaceEventMaxLimit     = 500
)

type workspaceEventResponse struct {
	ID            string          `json:"id"`
	WorkspaceID   string          `json:"workspace_id"`
	Sequence      int64           `json:"sequence"`
	SourceID      string          `json:"source_id"`
	Type          string          `json:"type"`
	AggregateKind string          `json:"aggregate_kind"`
	AggregateID   string          `json:"aggregate_id"`
	ActorType     *string         `json:"actor_type,omitempty"`
	ActorID       *string         `json:"actor_id,omitempty"`
	OccurredAt    string          `json:"occurred_at"`
	Payload       json.RawMessage `json:"payload"`
}

type workspaceEventPage struct {
	Events     []workspaceEventResponse `json:"events"`
	NextCursor string                   `json:"next_cursor"`
	HasMore    bool                     `json:"has_more"`
	Types      []string                 `json:"types,omitempty"`
}

type workspaceEventCursorToken struct {
	Version  int      `json:"v"`
	Sequence int64    `json:"sequence"`
	Types    []string `json:"types"`
}

// ListWorkspaceEvents returns committed events after a workspace sequence.
// Unfiltered cursors are decimal workspace sequences. Filtered cursors embed
// the normalized type set, so changing a filter cannot silently reuse an
// incompatible position and skip newly matching events.
func (h *Handler) ListWorkspaceEvents(w http.ResponseWriter, r *http.Request) {
	eventTypes, err := parseWorkspaceEventTypes(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid event type filter")
		return
	}
	after, err := parseWorkspaceEventCursor(r.URL.Query().Get("cursor"), eventTypes)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, err := parseWorkspaceEventLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid limit")
		return
	}

	workspaceID := middleware.WorkspaceIDFromContext(r.Context())
	workspaceUUID := parseUUID(workspaceID)
	bounds, err := h.Queries.GetWorkspaceEventBounds(r.Context(), workspaceUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to inspect workspace event retention")
		return
	}
	if after > bounds.MaxSequence {
		writeError(w, http.StatusBadRequest, "cursor is ahead of workspace event stream")
		return
	}
	if after > 0 && bounds.MinSequence > 0 && after < bounds.MinSequence-1 {
		writeJSON(w, http.StatusGone, map[string]any{
			"error":         "cursor expired",
			"code":          "cursor_expired",
			"oldest_cursor": formatWorkspaceEventCursor(bounds.MinSequence-1, eventTypes),
		})
		return
	}
	rows, err := h.Queries.ListWorkspaceEventsAfter(r.Context(), db.ListWorkspaceEventsAfterParams{
		WorkspaceID:   workspaceUUID,
		AfterSequence: after,
		EventTypes:    eventTypes,
		ResultLimit:   int32(limit + 1),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list workspace events")
		return
	}

	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	events := make([]workspaceEventResponse, 0, len(rows))
	next := after
	for _, row := range rows {
		payload := json.RawMessage(row.Payload)
		if len(payload) == 0 || !json.Valid(payload) {
			payload = json.RawMessage(`{}`)
		}
		events = append(events, workspaceEventResponse{
			ID:            uuidToString(row.ID),
			WorkspaceID:   uuidToString(row.WorkspaceID),
			Sequence:      row.Sequence,
			SourceID:      row.SourceID,
			Type:          row.EventType,
			AggregateKind: row.AggregateKind,
			AggregateID:   uuidToString(row.AggregateID),
			ActorType:     textToPtr(row.ActorType),
			ActorID:       uuidToPtr(row.ActorID),
			OccurredAt:    timestampToString(row.OccurredAt),
			Payload:       payload,
		})
		next = row.Sequence
	}
	// Once every matching row in the current snapshot fits in this page,
	// advance across unmatched sequences too. Filtered cursors are bound to
	// their exact type set, so this cannot skip events for a later filter; it
	// prevents a quiet filtered watcher from rescanning an ever-growing tail.
	if !hasMore && bounds.MaxSequence > next {
		next = bounds.MaxSequence
	}

	writeJSON(w, http.StatusOK, workspaceEventPage{
		Events:     events,
		NextCursor: formatWorkspaceEventCursor(next, eventTypes),
		HasMore:    hasMore,
		Types:      eventTypes,
	})
}

func parseWorkspaceEventCursor(raw string, eventTypes []string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	if value, err := strconv.ParseInt(raw, 10, 64); err == nil && value >= 0 {
		if len(eventTypes) > 0 && value != 0 {
			return 0, &workspaceEventCursorError{message: "cursor does not match event type filter"}
		}
		return value, nil
	}
	if !strings.HasPrefix(raw, "evt1.") {
		return 0, &workspaceEventCursorError{message: "invalid cursor"}
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(raw, "evt1."))
	if err != nil {
		return 0, &workspaceEventCursorError{message: "invalid cursor"}
	}
	var token workspaceEventCursorToken
	if err := json.Unmarshal(data, &token); err != nil || token.Version != 1 || token.Sequence < 0 {
		return 0, &workspaceEventCursorError{message: "invalid cursor"}
	}
	normalizedTokenTypes := normalizeWorkspaceEventTypes(token.Types)
	if !slices.Equal(normalizedTokenTypes, token.Types) || !slices.Equal(token.Types, eventTypes) {
		return 0, &workspaceEventCursorError{message: "cursor does not match event type filter"}
	}
	return token.Sequence, nil
}

type workspaceEventCursorError struct{ message string }

func (e *workspaceEventCursorError) Error() string { return e.message }

func formatWorkspaceEventCursor(sequence int64, eventTypes []string) string {
	if len(eventTypes) == 0 {
		return strconv.FormatInt(sequence, 10)
	}
	data, _ := json.Marshal(workspaceEventCursorToken{Version: 1, Sequence: sequence, Types: eventTypes})
	return "evt1." + base64.RawURLEncoding.EncodeToString(data)
}

func parseWorkspaceEventTypes(values url.Values) ([]string, error) {
	raw := append([]string(nil), values["type"]...)
	for _, value := range values["types"] {
		raw = append(raw, strings.Split(value, ",")...)
	}
	if len(raw) > 32 {
		return nil, strconv.ErrSyntax
	}
	for _, value := range raw {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 128 {
			return nil, strconv.ErrSyntax
		}
	}
	return normalizeWorkspaceEventTypes(raw), nil
}

func normalizeWorkspaceEventTypes(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func parseWorkspaceEventLimit(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return workspaceEventDefaultLimit, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > workspaceEventMaxLimit {
		return 0, strconv.ErrSyntax
	}
	return value, nil
}
