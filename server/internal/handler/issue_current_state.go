package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	issueCurrentStateMetadataKey = "current_state"
	maxCurrentStateFieldBytes    = 4096
	maxCurrentStateRecordBytes   = 7000
)

// IssueCurrentState is the small live handoff stored at metadata.current_state.
// The update is a full snapshot: every field is present on every write, while
// blocker and expiry can be explicitly cleared. Provenance is stamped by the
// server in the same database update.
type IssueCurrentState struct {
	State         string                 `json:"state"`
	Blocker       string                 `json:"blocker"`
	NextAction    string                 `json:"next_action"`
	Owner         string                 `json:"owner"`
	Expiry        *string                `json:"expiry"`
	UpdatedAt     string                 `json:"updated_at"`
	UpdatedBy     IssueCurrentStateActor `json:"updated_by"`
	IssueRevision int64                  `json:"issue_revision"`
}

type IssueCurrentStateActor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type IssueCurrentStateUpdateRequest struct {
	State      string  `json:"state"`
	Blocker    string  `json:"blocker"`
	NextAction string  `json:"next_action"`
	Owner      string  `json:"owner"`
	Expiry     *string `json:"expiry"`
}

type issueCurrentStateResponse struct {
	CurrentState  *IssueCurrentState `json:"current_state"`
	IssueRevision int64              `json:"issue_revision,omitempty"`
}

func decodeIssueCurrentStateUpdate(body io.Reader) (IssueCurrentStateUpdateRequest, error) {
	var raw map[string]json.RawMessage
	decoder := json.NewDecoder(body)
	if err := decoder.Decode(&raw); err != nil {
		return IssueCurrentStateUpdateRequest{}, errors.New("request body must be a JSON object")
	}
	if raw == nil {
		return IssueCurrentStateUpdateRequest{}, errors.New("request body must be a JSON object")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return IssueCurrentStateUpdateRequest{}, errors.New("request body must contain one JSON object")
	}

	fields := []string{"state", "blocker", "next_action", "owner", "expiry"}
	for _, key := range fields {
		if _, ok := raw[key]; !ok {
			return IssueCurrentStateUpdateRequest{}, fmt.Errorf("%s is required; send a complete current-state snapshot", key)
		}
	}
	for key := range raw {
		found := false
		for _, expected := range fields {
			if key == expected {
				found = true
				break
			}
		}
		if !found {
			return IssueCurrentStateUpdateRequest{}, fmt.Errorf("unknown field %q", key)
		}
	}

	var request IssueCurrentStateUpdateRequest
	for key, destination := range map[string]*string{
		"state":       &request.State,
		"blocker":     &request.Blocker,
		"next_action": &request.NextAction,
		"owner":       &request.Owner,
	} {
		if err := decodeCurrentStateString(raw[key], destination); err != nil {
			return IssueCurrentStateUpdateRequest{}, fmt.Errorf("%s must be a string", key)
		}
	}
	if !bytes.Equal(bytes.TrimSpace(raw["expiry"]), []byte("null")) {
		var expiry string
		if err := decodeCurrentStateString(raw["expiry"], &expiry); err != nil {
			return IssueCurrentStateUpdateRequest{}, errors.New("expiry must be an RFC3339 string or null")
		}
		request.Expiry = &expiry
	}
	return normalizeIssueCurrentStateUpdate(request)
}

func decodeCurrentStateString(raw json.RawMessage, destination *string) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return errors.New("expected string")
	}
	return json.Unmarshal(trimmed, destination)
}

func normalizeIssueCurrentStateUpdate(request IssueCurrentStateUpdateRequest) (IssueCurrentStateUpdateRequest, error) {
	request.State = strings.TrimSpace(request.State)
	request.Blocker = strings.TrimSpace(request.Blocker)
	request.NextAction = strings.TrimSpace(request.NextAction)
	request.Owner = strings.TrimSpace(request.Owner)
	if request.State == "" {
		return IssueCurrentStateUpdateRequest{}, errors.New("state is required")
	}
	if len(request.State) > 128 {
		return IssueCurrentStateUpdateRequest{}, errors.New("state must be at most 128 bytes")
	}
	if strings.IndexByte(request.State, 0) >= 0 {
		return IssueCurrentStateUpdateRequest{}, errors.New("state cannot contain a NUL byte")
	}
	for field, value := range map[string]string{
		"blocker": request.Blocker, "next_action": request.NextAction, "owner": request.Owner,
	} {
		if len(value) > maxCurrentStateFieldBytes {
			return IssueCurrentStateUpdateRequest{}, fmt.Errorf("%s must be at most %d bytes", field, maxCurrentStateFieldBytes)
		}
		if strings.IndexByte(value, 0) >= 0 {
			return IssueCurrentStateUpdateRequest{}, fmt.Errorf("%s cannot contain a NUL byte", field)
		}
	}
	if request.Expiry != nil {
		value := strings.TrimSpace(*request.Expiry)
		if value == "" {
			request.Expiry = nil
		} else {
			expires, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return IssueCurrentStateUpdateRequest{}, errors.New("expiry must be an RFC3339 timestamp or empty")
			}
			canonical := expires.UTC().Format(time.RFC3339Nano)
			request.Expiry = &canonical
		}
	}
	return request, nil
}

func issueCurrentStateFromMetadata(raw []byte) (*IssueCurrentState, error) {
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, fmt.Errorf("decode issue metadata: %w", err)
	}
	encoded, ok := metadata[issueCurrentStateMetadataKey]
	if !ok || bytes.Equal(bytes.TrimSpace(encoded), []byte("null")) {
		return nil, nil
	}
	// `current_state` was a valid primitive metadata key before the structured
	// record shipped. Keep dedicated reads compatible with those rows instead
	// of trying to decode a string/number/bool as the new record.
	trimmed := bytes.TrimSpace(encoded)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, nil
	}
	var state IssueCurrentState
	if err := json.Unmarshal(encoded, &state); err != nil {
		return nil, fmt.Errorf("decode stored current state: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, fmt.Errorf("decode stored current state: %w", err)
	}
	for _, key := range []string{"state", "blocker", "next_action", "owner", "expiry", "updated_at", "updated_by", "issue_revision"} {
		if _, ok := fields[key]; !ok {
			return nil, fmt.Errorf("stored current state is missing %s", key)
		}
	}
	if state.State == "" || state.UpdatedBy.Type == "" || state.UpdatedBy.ID == "" || state.IssueRevision < 1 {
		return nil, errors.New("stored current state has incomplete provenance or state")
	}
	if _, err := normalizeIssueCurrentStateUpdate(IssueCurrentStateUpdateRequest{
		State: state.State, Blocker: state.Blocker, NextAction: state.NextAction,
		Owner: state.Owner, Expiry: state.Expiry,
	}); err != nil {
		return nil, fmt.Errorf("stored current state has invalid fields: %w", err)
	}
	if _, err := time.Parse(time.RFC3339Nano, state.UpdatedAt); err != nil {
		return nil, errors.New("stored current state has an invalid updated_at")
	}
	if state.Expiry != nil {
		if _, err := time.Parse(time.RFC3339Nano, *state.Expiry); err != nil {
			return nil, errors.New("stored current state has an invalid expiry")
		}
	}
	return &state, nil
}

// issueMetadataForResponse keeps the long-standing primitive-only metadata
// wire contract for installed clients. The structured record is returned in
// the additive top-level current_state field instead. Legacy primitive values
// under the old metadata key stay in metadata until callers migrate them.
func issueMetadataForResponse(raw []byte) (map[string]any, *IssueCurrentState) {
	metadata := parseIssueMetadata(raw)
	if _, structured := metadata[issueCurrentStateMetadataKey].(map[string]any); !structured {
		return metadata, nil
	}
	delete(metadata, issueCurrentStateMetadataKey)
	state, err := issueCurrentStateFromMetadata(raw)
	if err != nil {
		return metadata, nil
	}
	return metadata, state
}

func (h *Handler) GetIssueCurrentState(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	state, err := issueCurrentStateFromMetadata(issue.Metadata)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "stored issue current state is invalid")
		return
	}
	writeJSON(w, http.StatusOK, issueCurrentStateResponse{CurrentState: state, IssueRevision: issue.Revision})
}

func (h *Handler) SetIssueCurrentState(w http.ResponseWriter, r *http.Request) {
	r = h.withWakeupActor(r)
	request, err := decodeIssueCurrentStateUpdate(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	metadata := parseIssueMetadata(issue.Metadata)
	if _, present := metadata[issueCurrentStateMetadataKey]; !present && len(metadata) >= maxIssueMetadataKeys {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("metadata cannot exceed %d keys", maxIssueMetadataKeys))
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	actorType, actorID := h.resolveActor(r, userID, uuidToString(issue.WorkspaceID))
	if actorType == "" || actorID == "" {
		writeError(w, http.StatusInternalServerError, "could not resolve current-state update actor")
		return
	}

	stateJSON, err := json.Marshal(map[string]any{
		"state": request.State, "blocker": request.Blocker,
		"next_action": request.NextAction, "owner": request.Owner,
		"expiry":     request.Expiry,
		"updated_by": IssueCurrentStateActor{Type: actorType, ID: actorID},
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encode current state")
		return
	}
	if len(stateJSON) > maxCurrentStateRecordBytes {
		writeError(w, http.StatusBadRequest, "current-state record exceeds the size limit")
		return
	}

	updated, err := wakeupWrite(h, r, func(q *db.Queries) (db.SetIssueCurrentStateRow, error) {
		return q.SetIssueCurrentState(r.Context(), db.SetIssueCurrentStateParams{
			ID: issue.ID, WorkspaceID: issue.WorkspaceID, CurrentState: stateJSON,
		})
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "issue not found")
			return
		}
		if isCheckViolation(err) {
			writeError(w, http.StatusBadRequest, "current-state record exceeds the metadata size limit")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to update issue current state")
		return
	}
	state, err := issueCurrentStateFromMetadata(updated.Metadata)
	if err != nil || state == nil {
		writeError(w, http.StatusInternalServerError, "failed to read updated issue current state")
		return
	}
	state.IssueRevision = updated.Revision
	state.UpdatedAt = timestampToString(updated.UpdatedAt)

	workspaceID := uuidToString(issue.WorkspaceID)
	metadata, _ = issueMetadataForResponse(updated.Metadata)
	h.publish(protocol.EventIssueMetadataChanged, workspaceID, actorType, actorID, map[string]any{
		"issue_id":       uuidToString(issue.ID),
		"metadata":       metadata,
		"current_state":  state,
		"issue_revision": updated.Revision,
	})
	writeJSON(w, http.StatusOK, issueCurrentStateResponse{CurrentState: state, IssueRevision: updated.Revision})
}
