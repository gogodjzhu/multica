package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
)

func runSideQuestionSweeper(ctx context.Context, tasks *service.TaskService) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			if err := tasks.SweepChatSideQuestions(sweepCtx, 50); err != nil {
				slog.Warn("side question cleanup failed", "error", err)
			}
			cancel()
		}
	}
}
