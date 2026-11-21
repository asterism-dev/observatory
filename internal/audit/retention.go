package audit

import (
	"context"
	"log/slog"
	"time"
)

const (
	// ponytail: fixed until the platform-wide retention setting exists; it replaces this constant.
	defaultRetentionDays = 365
	retentionInterval    = 24 * time.Hour
)

// runRetention deletes events older than days at start and then daily; concurrent instances only repeat the same delete.
func runRetention(ctx context.Context, s *store, days int) {
	for {
		n, err := s.deleteBefore(ctx, time.Now().AddDate(0, 0, -days))
		if err != nil && ctx.Err() == nil {
			slog.Error("audit retention failed", "err", err)
		} else if n > 0 {
			slog.Info("audit retention", "deleted", n, "days", days)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retentionInterval):
		}
	}
}
