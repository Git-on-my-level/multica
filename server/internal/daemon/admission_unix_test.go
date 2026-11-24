//go:build !windows

package daemon

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/pkg/agent"
)

func TestAdmissionOpenCodeKeepsCancellationAndTimeoutClassification(t *testing.T) {
	for _, want := range []string{"cancelled", "timeout"} {
		t.Run(want, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "opencode")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat >/dev/null\nwhile :; do sleep 1; done\n"), 0700); err != nil {
				t.Fatal(err)
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			backend, err := agent.New("opencode", agent.Config{ExecutablePath: bin, Logger: logger,
				Env: map[string]string{"MULTICA_OPENCODE_TRANSPORT": "run"}})
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if want == "timeout" {
				var stopDeadline context.CancelFunc
				ctx, stopDeadline = context.WithTimeout(ctx, 2*time.Second)
				defer stopDeadline()
			}
			fake := &fakeAdmission{granted: true}
			controller := newAdmissionController(fake, logger)
			lease, allowed, err := controller.reserve(ctx, "rt", "morph/deepseek")
			if !allowed || err != nil || lease == nil {
				t.Fatalf("reserve = %v, %t, %v", lease, allowed, err)
			}
			allowExit, exited := make(chan struct{}), make(chan struct{})
			unblockExit := sync.OnceFunc(func() { close(allowExit) })
			defer unblockExit()

			d := &Daemon{logger: logger}
			result, _, err := d.executeAndDrain(ctx, backend, "test", agent.ExecOptions{
				Cwd: dir, OpenCodeAttempt: lease.countAttempt,
				OpenCodeProcessStarted: func() func() {
					finish := lease.processStarted()
					if want == "cancelled" {
						cancel()
					}
					return func() {
						<-allowExit
						finish()
						close(exited)
					}
				},
			}, logger, "task", "", new(atomic.Int32))
			if err != nil || result.Status != want {
				t.Fatalf("result status = %q, err = %v, want %q", result.Status, err, want)
			}
			lease.release()
			if fake.releases != 0 {
				t.Fatal("classification returned the slot before process cleanup")
			}
			unblockExit()
			select {
			case <-exited:
				if fake.releases != 1 {
					t.Fatalf("release count = %d, want 1", fake.releases)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("admitted process did not finish cleanup")
			}
		})
	}
}

func TestAdmissionStartupLogsResolvedAgentctl(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agentctl")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	var logs bytes.Buffer
	d := &Daemon{
		cfg:    Config{Admission: cli.AdmissionConfig{Enabled: true}},
		logger: slog.New(slog.NewTextHandler(&logs, nil)),
	}
	d.startAdmission()
	if got := d.admission.client.(agentctlAdmission).path; got != bin {
		t.Fatalf("agentctl path = %q, want %q", got, bin)
	}
	if log := logs.String(); !strings.Contains(log, "level=INFO") ||
		strings.Count(log, "OpenCode admission enabled") != 1 ||
		!strings.Contains(log, "agentctl_path="+bin) {
		t.Fatalf("unexpected admission startup log: %s", log)
	}
}
