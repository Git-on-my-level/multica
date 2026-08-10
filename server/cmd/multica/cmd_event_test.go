package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func newEventTestCommand(serverURL string) *cobra.Command {
	cmd := &cobra.Command{Use: "event-test"}
	cmd.Flags().String("server-url", serverURL, "")
	cmd.Flags().String("workspace-id", "11111111-1111-4111-8111-111111111111", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("cursor", "0", "")
	cmd.Flags().Int("limit", 100, "")
	cmd.Flags().Duration("interval", 100*time.Millisecond, "")
	cmd.Flags().StringSlice("type", nil, "")
	_ = cmd.Flags().Set("server-url", serverURL)
	_ = cmd.Flags().Set("workspace-id", "11111111-1111-4111-8111-111111111111")
	return cmd
}

func TestRunEventListUsesCursorAndPrintsPage(t *testing.T) {
	t.Setenv("MULTICA_TOKEN", "test-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-Workspace-ID"); got != "11111111-1111-4111-8111-111111111111" {
			t.Errorf("X-Workspace-ID = %q", got)
		}
		if got := r.URL.Query().Get("cursor"); got != "41" {
			t.Errorf("cursor = %q", got)
		}
		if got := r.URL.Query().Get("limit"); got != "2" {
			t.Errorf("limit = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"events":[{"id":"e1","workspace_id":"w1","sequence":42,"source_id":"s1","type":"issue:updated","aggregate_kind":"issue","aggregate_id":"a1","occurred_at":"2026-08-10T00:00:00Z","payload":{}}],"next_cursor":"42","has_more":false}`))
	}))
	defer server.Close()

	cmd := newEventTestCommand(server.URL)
	_ = cmd.Flags().Set("cursor", "41")
	_ = cmd.Flags().Set("limit", "2")
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runEventList(cmd, nil); err != nil {
		t.Fatalf("runEventList: %v", err)
	}
	var page eventPage
	if err := json.Unmarshal(out.Bytes(), &page); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if len(page.Events) != 1 || page.NextCursor != "42" || page.Events[0].Sequence != 42 {
		t.Fatalf("page = %+v", page)
	}
}

func TestRunEventWatchIsQuietOnEmptyPolls(t *testing.T) {
	t.Setenv("MULTICA_TOKEN", "test-token")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`{"events":[{"id":"e1","workspace_id":"w1","sequence":7,"source_id":"s1","type":"task:completed","aggregate_kind":"task","aggregate_id":"a1","occurred_at":"2026-08-10T00:00:00Z","payload":{}}],"next_cursor":"7","has_more":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"events":[],"next_cursor":"7","has_more":false}`))
		cancel()
	}))
	defer server.Close()

	cmd := newEventTestCommand(server.URL)
	cmd.SetContext(ctx)
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runEventWatch(cmd, nil); err != nil {
		t.Fatalf("runEventWatch: %v", err)
	}
	lines := bytes.Count(bytes.TrimSpace(out.Bytes()), []byte("\n")) + 1
	if lines != 1 {
		t.Fatalf("watch wrote %d lines for one event: %q", lines, out.String())
	}
	var event eventRecord
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &event); err != nil {
		t.Fatalf("decode watch line: %v", err)
	}
	if event.Sequence != 7 || strconv.FormatInt(event.Sequence, 10) != "7" {
		t.Fatalf("event = %+v", event)
	}
}
