//go:build unix

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCodexInitializeParentDeadlineDoesNotPersistOpaqueEnv is the other half of
// the parent-context pair: codex.go treats a parent DEADLINE and a parent
// CANCELLATION as one kind of ending (`contextEnded` in the initialize-failure
// path, codex.go), and both must keep untrusted child stderr out of the Result —
// that stderr can echo opaque Config.Env/auth values which pattern sanitization
// cannot recognize.
//
// Not a transport-timeout test, and it must not be deleted as one: the deadline
// here is the CALLER's own budget, and it is the only thing that exercises the
// DeadlineExceeded arm of `errors.Is(err, context.DeadlineExceeded) ||
// errors.Is(err, context.Canceled)`. The sibling cancellation test covers the
// Canceled arm only.
//
// The fake writes the secret to stderr before reading the handshake, so the
// redaction check never depends on racing our own initialize write. The
// caller's deadline expires only once the fake has written it, and the
// handshake bound is the fallback timeout, so the deadline is the only thing
// that can end the run no matter how slowly the fake starts.
func TestCodexInitializeParentDeadlineDoesNotPersistOpaqueEnv(t *testing.T) {
	t.Parallel()

	const secret = "opaque-init-parent-deadline-sentinel-8842"
	readyFile := filepath.Join(t.TempDir(), "stderr-written")
	fakePath := writeFakeCodexAppServer(t, ""+
		`echo "$OPAQUE_AUTH_VALUE" >&2`+"\n"+
		`touch "`+readyFile+`"`+"\n"+
		`read line`+"\n"+
		`sleep 300`+"\n")

	var logs strings.Builder
	backend, err := New("codex", Config{
		ExecutablePath: fakePath,
		Env:            map[string]string{"OPAQUE_AUTH_VALUE": secret},
		Logger:         slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := newExpirableContext()
	defer ctx.expire()
	session, err := backend.Execute(ctx, "prompt", ExecOptions{Timeout: testFallbackTimeout, HandshakeTimeout: testFallbackTimeout})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range session.Messages {
		}
	}()
	waitForTestFile(t, readyFile)
	ctx.expire()
	result := <-session.Result
	persisted := fmt.Sprintf("%+v\n%s", result, logs.String())
	if strings.Contains(persisted, secret) {
		t.Fatalf("opaque env persisted after the parent deadline: %s", persisted)
	}
	if result.Status != "failed" {
		t.Fatalf("parent deadline must fail without initialize retry: %+v", result)
	}
}

func TestCodexInitializeParentCancellationDoesNotPersistOpaqueEnv(t *testing.T) {
	t.Parallel()

	const secret = "opaque-init-parent-context-sentinel-8841"
	readyFile := filepath.Join(t.TempDir(), "stderr-written")
	fakePath := writeFakeCodexAppServer(t, ""+
		`read line`+"\n"+
		`echo "$OPAQUE_AUTH_VALUE" >&2`+"\n"+
		`touch "`+readyFile+`"`+"\n"+
		`sleep 300`+"\n")

	var logs strings.Builder
	backend, err := New("codex", Config{
		ExecutablePath: fakePath,
		Env:            map[string]string{"OPAQUE_AUTH_VALUE": secret},
		Logger:         slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session, err := backend.Execute(ctx, "prompt", ExecOptions{Timeout: testFallbackTimeout, HandshakeTimeout: testFallbackTimeout})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range session.Messages {
		}
	}()
	waitForTestFile(t, readyFile)
	cancel()
	result := <-session.Result
	persisted := fmt.Sprintf("%+v\n%s", result, logs.String())
	if strings.Contains(persisted, secret) {
		t.Fatalf("opaque env persisted after parent context ended: %s", persisted)
	}
	if result.Status != "failed" {
		t.Fatalf("parent context must fail without initialize retry: %+v", result)
	}
}

func TestCodexInitializeTimeoutDoesNotReplayAmbiguousStartup(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		startup string
		effects int
	}{
		{name: "silent"},
		{
			name:    "silent after a local side effect",
			startup: `printf 'effect\n' >> "$STARTUP_EFFECTS"`,
			effects: 1,
		},
		{
			name:    "activity without an initialize response",
			startup: `echo '{"jsonrpc":"2.0","method":"account/updated","params":{"authMode":"chatgpt"}}'`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			launchesPath := filepath.Join(root, "launches")
			effectsPath := filepath.Join(root, "effects")
			pidsPath := filepath.Join(root, "pids")
			fakePath := writeFakeCodexAppServer(t, `
printf 'launch\n' >> "$LAUNCHES"
read -r request || exit 1
case "$request" in *'"method":"initialize"'*) ;; *) exit 2 ;; esac
`+tc.startup+`
sleep 30 </dev/null >/dev/null 2>&1 &
printf '%s %s\n' "$$" "$!" > "$PIDS"
wait
`)
			var logs strings.Builder
			result, messages := executeFakeCodexCollectingMessagesWithConfig(t, fakePath, Config{
				Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
				TaskID: "initialize-timeout-task",
				Env: map[string]string{
					"LAUNCHES": launchesPath, "STARTUP_EFFECTS": effectsPath, "PIDS": pidsPath,
				},
			}, ExecOptions{HandshakeTimeout: 2 * time.Second}, 15*time.Second)

			if result.Status != "failed" || !strings.Contains(result.Error, "codex initialize failed: "+CodexHandshakeTimeoutMarker) {
				t.Fatalf("expected terminal initialize timeout, got %+v", result)
			}
			if len(messages) != 0 {
				t.Fatalf("unexpected task messages: %+v", messages)
			}
			launches, err := os.ReadFile(launchesPath)
			if err != nil || string(launches) != "launch\n" {
				t.Fatalf("expected a single launch, got %q (%v)", launches, err)
			}
			effects, err := os.ReadFile(effectsPath)
			if tc.effects == 0 && !os.IsNotExist(err) {
				t.Fatalf("expected no fixture effects, got %q (%v)", effects, err)
			}
			if tc.effects == 1 && (err != nil || string(effects) != "effect\n") {
				t.Fatalf("expected one fixture effect, got %q (%v)", effects, err)
			}
			pidBytes, err := os.ReadFile(pidsPath)
			pids := strings.Fields(string(pidBytes))
			if err != nil || len(pids) != 2 {
				t.Fatalf("expected leader and descendant PIDs, got %q (%v)", pidBytes, err)
			}
			for _, value := range pids {
				pid, err := strconv.Atoi(value)
				if err != nil || pid <= 0 {
					t.Fatalf("invalid PID %q", value)
				}
				waitProcessGone(t, pid)
			}
			logged := logs.String()
			for _, want := range []string{`"phase":"cleanup"`, `"phase":"initialize_failure"`, `"task_id":"initialize-timeout-task"`, `"attempt":1`} {
				if !strings.Contains(logged, want) {
					t.Errorf("missing %q in lifecycle log: %s", want, logged)
				}
			}
		})
	}
}
