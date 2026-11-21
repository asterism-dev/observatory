package audit

import (
	"context"
	"log/slog"
	"time"
)

const retentionInterval = 24 * time.Hour

// runRetention expires events at start and then daily; concurrent instances only repeat the same delete.
func runRetention(ctx context.Context, s *store) {
	for {
		if n, err := expire(ctx, s); err != nil && ctx.Err() == nil {
			slog.Error("audit retention failed", "err", err)
		} else if n > 0 {
			slog.Info("audit retention", "deleted", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retentionInterval):
		}
	}
}

func expire(ctx context.Context, s *store) (int64, error) {
	days, err := s.retentionDays(ctx)
	if err != nil {
		return 0, err
	}
	return s.deleteBefore(ctx, time.Now().AddDate(0, 0, -days))
}
