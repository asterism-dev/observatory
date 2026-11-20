package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	DeleteReasonUser    = "user"
	DeleteReasonExpired = "expired"
)

// VMHistory is a deleted VM; the user, team, node and creator ids turn nil once those are deleted.
type VMHistory struct {
	ID, OrgID                                    uuid.UUID
	UserID, TeamID, NodeID, CreatedBy, DeletedBy *uuid.UUID
	Name, ImageRef, Reason                       string
	CPUs, MemoryMB, DiskGB                       int
	CreatedAt, DeletedAt                         time.Time
}

func (s *Store) UserTeamIDs(ctx context.Context, org, user uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.q.Query(ctx, "SELECT t.id FROM teams t JOIN team_members tm ON tm.team_id = t.id WHERE t.org_id = $1 AND tm.user_id = $2", org, user)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// VMHistory lists the org's deleted VMs, the latest deletion first.
func (s *Store) VMHistory(ctx context.Context, org uuid.UUID) ([]VMHistory, error) {
	rows, err := s.q.Query(ctx, `
		SELECT id, org_id, user_id, team_id, node_id, created_by, deleted_by, name, image_ref, reason, cpus, memory_mb, disk_gb, created_at, deleted_at
		FROM vm_history WHERE org_id = $1 ORDER BY deleted_at DESC`, org)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (h VMHistory, err error) {
		err = r.Scan(&h.ID, &h.OrgID, &h.UserID, &h.TeamID, &h.NodeID, &h.CreatedBy, &h.DeletedBy, &h.Name, &h.ImageRef, &h.Reason,
			&h.CPUs, &h.MemoryMB, &h.DiskGB, &h.CreatedAt, &h.DeletedAt)
		return
	})
}
