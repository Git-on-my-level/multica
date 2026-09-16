package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
)

func runChildAttentionSweeper(ctx context.Context, tasks *service.TaskService) {
	runPeriodicSweep(ctx, 5*time.Second, func() {
		bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		if _, err := tasks.RecoverChildAttention(bounded, 32); err != nil {
			slog.Warn("child attention: recovery sweep failed", "error", err)
		}
	})
}
