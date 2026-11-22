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
func (s *Store) OwnerRefs(ctx context.Context) ([]authz.VMRef, []authz.DiskRef, error) {
	rows, err := s.q.Query(ctx, `SELECT org_id, id, user_id, team_id FROM vms`)
	if err != nil {
		return nil, nil, err
	}
	vms, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (v authz.VMRef, err error) {
		err = r.Scan(&v.OrgID, &v.ID, &v.UserID, &v.TeamID)
		return
	})
	if err != nil {
		return nil, nil, err
	}
	rows, err = s.q.Query(ctx, `SELECT org_id, id, user_id, team_id FROM disks WHERE deleted_at IS NULL`)
	if err != nil {
		return nil, nil, err
	}
	disks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (d authz.DiskRef, err error) {
		err = r.Scan(&d.OrgID, &d.ID, &d.UserID, &d.TeamID)
		return
	})
	return vms, disks, err
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
