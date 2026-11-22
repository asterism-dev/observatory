package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/asterism-dev/observatory/internal/authz"
)

// Role is an org role as iam's policy names it.
type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

// OwnerRefs reads the owners of VMs and live disks, which grant their owners access.
func (s *Store) OwnerRefs(ctx context.Context) ([]authz.OwnedRef, error) {
	rows, err := s.q.Query(ctx, `SELECT 'vms', org_id, id, user_id, team_id FROM vms
		UNION ALL SELECT 'disks', org_id, id, user_id, team_id FROM disks WHERE deleted_at IS NULL`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (o authz.OwnedRef, err error) {
		err = r.Scan(&o.Kind, &o.OrgID, &o.ID, &o.UserID, &o.TeamID)
		return
	})
}

// LockHubOrg serializes the hub's resource writes in an org (quotas, addresses, attachments); the row is created on first use.
func (s *Store) LockHubOrg(ctx context.Context, orgID uuid.UUID) error {
	if _, err := s.q.Exec(ctx, "INSERT INTO hub_orgs (org_id) VALUES ($1) ON CONFLICT DO NOTHING", orgID); err != nil {
		return err
	}
	_, err := s.q.Exec(ctx, "SELECT 1 FROM hub_orgs WHERE org_id = $1 FOR UPDATE", orgID)
	return err
}

// lockUserScope orders a user-scope create against PurgeMember, which holds LockHubOrg.
func (s *Store) lockUserScope(ctx context.Context, orgID uuid.UUID, scope EnvScope) error {
	if scope != EnvScopeUser {
		return nil
	}
	return s.LockHubOrg(ctx, orgID)
}
