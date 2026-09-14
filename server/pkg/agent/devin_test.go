package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDevinIsACPFamily(t *testing.T) {
	t.Parallel()

	desc, ok := BuiltinRuntimeByID("devin")
	if !ok {
		t.Fatal("devin missing from BuiltinRuntimes")
	}
	if desc.ProtocolFamily != "devin" || desc.DefaultCommand != "devin" {
		t.Fatalf("unexpected descriptor: %+v", desc)
	}
	if desc.LaunchHeader != "devin acp" {
		t.Fatalf("launch header = %q, want devin acp", desc.LaunchHeader)
	}
	if desc.ModelDiscovery == nil {
		t.Fatal("devin ModelDiscovery is nil")
	}

	b, err := New("devin", Config{Logger: slog.Default(), ExecutablePath: "devin"})
	if err != nil {
		t.Fatalf("New(devin): %v", err)
	}
	if _, ok := b.(*devinBackend); !ok {
		t.Fatalf("New(devin) = %T, want *devinBackend", b)
	}

	resolved, err := ResolveBackend("devin", Config{Logger: slog.Default()})
	if err != nil {
		t.Fatalf("ResolveBackend(devin): %v", err)
	}
	if _, ok := resolved.(*devinBackend); !ok {
		t.Fatalf("ResolveBackend(devin) = %T, want *devinBackend", resolved)
	}
}

func fakeDevinACPScript() string {
	return `#!/bin/sh
if [ -n "$DEVIN_ARGS_FILE" ]; then
  for arg in "$@"; do
    printf '%s\n' "$arg" >> "$DEVIN_ARGS_FILE"
  done
fi
while IFS= read -r line; do
  if [ -n "$DEVIN_REQUESTS_FILE" ]; then
    printf '%s\n' "$line" >> "$DEVIN_REQUESTS_FILE"
  fi
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true,"mcpCapabilities":{"http":true,"sse":true}}}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"ses_devin","models":{"currentModelId":"kimi-k2.5"}}}\n' "$id"
      ;;
    *'"method":"session/load"'*)
      if [ -n "$DEVIN_LOAD_NOT_FOUND" ]; then
        printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"Session not found: ses_gone"}}\n' "$id"
        exit 0
      fi
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"ses_loaded"}}\n' "$id"
      ;;
    *'"method":"session/prompt"'*)
      if [ -n "$DEVIN_PROMPT_NOT_FOUND" ]; then
        printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"Session not found: ses_loaded"}}\n' "$id"
        exit 0
      fi
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_devin","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"pong"}}}}\n'
      printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}\n' "$id"
      exit 0
      ;;
  esac
done
`
}

func TestDevinBackendStreamsAndCompletes(t *testing.T) {
	t.Parallel()
	fakePath := filepath.Join(t.TempDir(), "devin")
	writeTestExecutable(t, fakePath, []byte(fakeDevinACPScript()))

	backend, err := New("devin", Config{ExecutablePath: fakePath, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new devin backend: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	session, err := backend.Execute(ctx, "say pong", ExecOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var messages []Message
	done := make(chan struct{})
	go func() {
		defer close(done)
		for m := range session.Messages {
			messages = append(messages, m)
		}
	}()
	result := <-session.Result
	<-done

	if result.Status != "completed" {
		t.Fatalf("expected completed, got status=%q error=%q", result.Status, result.Error)
	}
	if !strings.Contains(result.Output, "pong") {
		t.Fatalf("output = %q, want pong", result.Output)
	}
	if result.SessionID != "ses_devin" {
		t.Fatalf("session id = %q, want ses_devin", result.SessionID)
	}
}

func TestDevinBlockedArgsFiltering(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	argsFile := filepath.Join(tempDir, "argv.txt")
	fakePath := filepath.Join(tempDir, "devin")
	writeTestExecutable(t, fakePath, []byte(fakeDevinACPScript()))

	backend, err := New("devin", Config{
		ExecutablePath: fakePath,
		Logger:         slog.Default(),
		Env:            map[string]string{"DEVIN_ARGS_FILE": argsFile},
	})
	if err != nil {
		t.Fatalf("new devin backend: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	session, err := backend.Execute(ctx, "task", ExecOptions{
		Timeout:    5 * time.Second,
		CustomArgs: []string{"acp", "--yolo", "--permission-mode", "bypass", "-p", "--add-dir", "/tmp/extra"},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()
	<-session.Result

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 1 || lines[0] != "acp" {
		t.Fatalf("argv = %q, want acp first", lines)
	}
	if c := countTokens(lines, "acp"); c != 1 {
		t.Errorf("expected exactly one acp, got %d (%q)", c, lines)
	}
	for _, blocked := range []string{"--yolo", "--permission-mode", "bypass", "-p"} {
		for _, got := range lines {
			if got == blocked {
				t.Errorf("blocked custom arg %q survived: %q", blocked, lines)
			}
		}
	}
	if !strings.Contains(strings.Join(lines, " "), "--add-dir /tmp/extra") {
		t.Errorf("expected allowed --add-dir to survive, got %q", lines)
	}
}

func TestDevinPassesModelFlag(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	argsFile := filepath.Join(tempDir, "argv.txt")
	fakePath := filepath.Join(tempDir, "devin")
	writeTestExecutable(t, fakePath, []byte(fakeDevinACPScript()))

	backend, err := New("devin", Config{
		ExecutablePath: fakePath,
		Logger:         slog.Default(),
		Env:            map[string]string{"DEVIN_ARGS_FILE": argsFile},
	})
	if err != nil {
		t.Fatalf("new devin backend: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "task", ExecOptions{
		Timeout: 5 * time.Second,
		Model:   "swe-1.7-lightning",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()
	<-session.Result
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	got := strings.Join(strings.Split(strings.TrimSpace(string(raw)), "\n"), " ")
	if got != "acp --model swe-1.7-lightning" {
		t.Fatalf("argv = %q, want acp --model swe-1.7-lightning", got)
	}
}

// fakeDevinIgnoresEOFScript answers initialize with an error, then keeps the
// process alive after stdin closes — the shape that deadlocked the old
// stdin.Close → Wait cleanup, where the deferred cancel sat behind a Wait
// that only the cancel could unblock.
func fakeDevinIgnoresEOFScript() string {
	return `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"boot failure"}}\n' "$id"
      break
      ;;
  esac
done
# Outlive the closed stdin: only context cancellation (SIGKILL) ends this.
exec sleep 60
`
}

// TestDevinCleanupKillsChildThatIgnoresStdinEOF is the regression for the
// defer-ordering deadlock: an early handshake failure used to run
// stdin.Close → cmd.Wait → cancel, so a child that ignored EOF kept Wait
// blocked forever and the cancellation was never reached. The fixed order
// cancels before Wait, so the channels must close promptly even though the
// fake child is still alive at that point.
func TestDevinCleanupKillsChildThatIgnoresStdinEOF(t *testing.T) {
	t.Parallel()
	fakePath := filepath.Join(t.TempDir(), "devin")
	writeTestExecutable(t, fakePath, []byte(fakeDevinIgnoresEOFScript()))

	backend, err := New("devin", Config{ExecutablePath: fakePath, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new devin backend: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	session, err := backend.Execute(ctx, "task", ExecOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range session.Messages {
		}
	}()

	result := <-session.Result
	if result.Status != "failed" {
		t.Fatalf("status = %q, want failed (error=%q)", result.Status, result.Error)
	}
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("session.Messages stayed open after failure — Wait() blocked on a child that ignores stdin EOF")
	}
}

// runDevinResume drives Execute with a recorded session id and returns the
// terminal Result. The fake devin's behaviour is selected through env vars.
func runDevinResume(t *testing.T, env map[string]string) Result {
	t.Helper()
	fakePath := filepath.Join(t.TempDir(), "devin")
	writeTestExecutable(t, fakePath, []byte(fakeDevinACPScript()))

	backend, err := New("devin", Config{ExecutablePath: fakePath, Logger: slog.Default(), Env: env})
	if err != nil {
		t.Fatalf("new devin backend: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	session, err := backend.Execute(ctx, "continue", ExecOptions{
		Timeout:         5 * time.Second,
		ResumeSessionID: "ses_gone",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()
	return <-session.Result
}

// TestDevinResumeRejectedAtLoad covers the resume-not-found path: session/load
// fails with a session-not-found error and the backend must report
// ResumeRejected=true so the daemon retries from a fresh session instead of
// replaying the dead id on every later turn.
func TestDevinResumeRejectedAtLoad(t *testing.T) {
	t.Parallel()
	result := runDevinResume(t, map[string]string{"DEVIN_LOAD_NOT_FOUND": "1"})
	if result.Status != "failed" {
		t.Fatalf("status = %q, want failed (error=%q)", result.Status, result.Error)
	}
	if !result.ResumeRejected {
		t.Fatalf("ResumeRejected = false, want true on session/load not found (error=%q)", result.Error)
	}
	if result.SessionID != "" {
		t.Fatalf("SessionID = %q, want empty so the daemon does not keep the dead pointer", result.SessionID)
	}
}

// TestDevinResumeRejectedAtPrompt covers the prompt-time variant: session/load
// echoes the requested id back even though the session is gone, so the stale
// id only fails at session/prompt. The backend must clear SessionID and set
// ResumeRejected=true for the daemon's fresh-session retry.
func TestDevinResumeRejectedAtPrompt(t *testing.T) {
	t.Parallel()
	result := runDevinResume(t, map[string]string{"DEVIN_PROMPT_NOT_FOUND": "1"})
	if result.Status != "failed" {
		t.Fatalf("status = %q, want failed (error=%q)", result.Status, result.Error)
	}
	if !result.ResumeRejected {
		t.Fatalf("ResumeRejected = false, want true on prompt-time session not found (error=%q)", result.Error)
	}
	if result.SessionID != "" {
		t.Fatalf("SessionID = %q, want empty so the daemon does not keep the dead pointer", result.SessionID)
	}
}

// TestDevinResumeNotRejectedOnSuccess pins the negative: a resume that lands
// must not flag ResumeRejected, or the daemon would fork a live conversation.
func TestDevinResumeNotRejectedOnSuccess(t *testing.T) {
	t.Parallel()
	result := runDevinResume(t, nil)
	if result.Status != "completed" {
		t.Fatalf("status = %q, want completed (error=%q)", result.Status, result.Error)
	}
	if result.ResumeRejected {
		t.Fatal("ResumeRejected = true on a successful resume, want false")
	}
	if result.SessionID != "ses_loaded" {
		t.Fatalf("SessionID = %q, want ses_loaded", result.SessionID)
	}
}

func TestSelectDevinAuthMethod(t *testing.T) {
	t.Parallel()
	if got, err := selectDevinAuthMethod(nil, false); err != nil || got != "" {
		t.Fatalf("empty methods: got %q err %v", got, err)
	}
	if got, err := selectDevinAuthMethod([]string{"windsurf-api-key"}, true); err != nil || got != "windsurf-api-key" {
		t.Fatalf("windsurf with key: got %q err %v", got, err)
	}
	if _, err := selectDevinAuthMethod([]string{"windsurf-api-key"}, false); err == nil {
		t.Fatal("windsurf without key should fail")
	}
	if got, err := selectDevinAuthMethod([]string{"cached-login"}, false); err != nil || got != "cached-login" {
		t.Fatalf("cached-login: got %q err %v", got, err)
	}
	if got, err := selectDevinAuthMethod([]string{"devin-browser"}, false); err != nil || got != "" {
		t.Fatalf("devin-browser should skip handshake: got %q err %v", got, err)
	}
	if got, err := selectDevinAuthMethod([]string{"devin-browser", "windsurf-api-key"}, false); err != nil || got != "" {
		t.Fatalf("browser+windsurf without key should skip handshake: got %q err %v", got, err)
	}
	if got, err := selectDevinAuthMethod([]string{"devin-browser", "windsurf-api-key"}, true); err != nil || got != "windsurf-api-key" {
		t.Fatalf("browser+windsurf with key: got %q err %v", got, err)
	}
}

func TestParseDevinModels(t *testing.T) {
	t.Parallel()
	got, err := parseDevinModels([]byte(`{"models":[{"id":"kimi-k2.5","name":"Kimi K2.5"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "kimi-k2.5" || got[0].Label != "Kimi K2.5" {
		t.Fatalf("got %+v", got)
	}
	got, err = parseDevinModels([]byte(`[{"id":"opus","label":"Opus"}]`))
	if err != nil || len(got) != 1 || got[0].Label != "Opus" {
		t.Fatalf("array form: %+v err %v", got, err)
	}
	got, err = parseDevinModels([]byte(`{"families":[{"slug":"kimi-k3","variants":[{"model_uid":"kimi-k3-high","label":"Kimi K3 High"}]}]}`))
	if err != nil || len(got) != 1 || got[0].ID != "kimi-k3-high" || got[0].Provider != "kimi-k3" {
		t.Fatalf("families form: %+v err %v", got, err)
	}
}
