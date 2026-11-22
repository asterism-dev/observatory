package quota

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

const (
	sweepInterval = time.Minute
	pruneInterval = 24 * time.Hour
	// tombstoneAge outlives the stream's 7-day max age, so no event that could revive a pruned resource is left to replay.
	tombstoneAge = 8 * 24 * time.Hour
)

// runSweep deletes expired holds every minute and prunes old tombstones at start and then daily, until ctx ends; concurrent instances only repeat the same deletes.
func runSweep(ctx context.Context, s *store) {
	var pruned time.Time
	for {
		if err := s.sweepReservations(ctx); err != nil && ctx.Err() == nil {
			slog.Error("quota: reservation sweep failed", "err", err)
		}
		if s.now().Sub(pruned) >= pruneInterval {
			if err := s.pruneTombstones(ctx); err != nil {
				if ctx.Err() == nil {
					slog.Error("quota: tombstone pruning failed", "err", err)
				}
			} else {
				pruned = s.now()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(sweepInterval):
		}
	}
}

func (s *store) sweepReservations(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM reservations WHERE expires_at <= $1", s.now()); err != nil {
		return fmt.Errorf("sweep reservations: %w", err)
	}
	return nil
}

func (s *store) pruneTombstones(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM allocations WHERE deleted AND updated_at < $1", s.now().Add(-tombstoneAge)); err != nil {
		return fmt.Errorf("prune tombstones: %w", err)
	}
	return nil
}
