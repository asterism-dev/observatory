package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
)

var scopedTables = []string{"image_defaults", "images", "env_vars", "integrations", "dashboards"}

// PurgeMember deletes the hub rows of a user who left org, unless they are a member again; running it again is harmless.
func (s *Store) PurgeMember(ctx context.Context, org, user uuid.UUID) (purged bool, err error) {
	err = s.InTx(ctx, func(tx *Store) error {
		// The org row lock conflicts with the key-share lock a membership insert takes, so a re-join can't interleave.
		if err := tx.LockOrg(ctx, org); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		var member bool
		if err := tx.q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM memberships WHERE org_id = $1 AND user_id = $2)", org, user).Scan(&member); err != nil || member {
			return err
		}
		if err := tx.purgeScoped(ctx, org, EnvScopeUser, "user_id", user, authz.UserSubject(user)); err != nil {
			return err
		}
		if _, err := tx.q.Exec(ctx, "DELETE FROM devices WHERE org_id = $1 AND user_id = $2", org, user); err != nil {
			return err
		}
		purged = true
		return nil
	})
	return purged && err == nil, err
}

// PurgeTeam deletes the hub rows of a deleted team and unlinks its deleted disks and VM history; running it again is harmless.
func (s *Store) PurgeTeam(ctx context.Context, org, team uuid.UUID) error {
	return s.InTx(ctx, func(tx *Store) error {
		if err := tx.purgeScoped(ctx, org, EnvScopeTeam, "team_id", team, authz.TeamSubject(team)); err != nil {
			return err
		}
		// A live disk can't lose its owner (CHECK); the ownership check keeps those from existing here.
		if _, err := tx.q.Exec(ctx, "UPDATE disks SET team_id = NULL WHERE org_id = $1 AND team_id = $2 AND deleted_at IS NOT NULL", org, team); err != nil {
			return err
		}
		_, err := tx.q.Exec(ctx, "UPDATE vm_history SET team_id = NULL WHERE org_id = $1 AND team_id = $2", org, team)
		return err
	})
}

func (s *Store) purgeScoped(ctx context.Context, org uuid.UUID, scope EnvScope, col string, id uuid.UUID, subject string) error {
	for _, table := range scopedTables {
		if _, err := s.q.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE org_id = $1 AND scope = $2 AND %s = $3", table, col), org, scope, id); err != nil {
			return fmt.Errorf("purge %s: %w", table, err)
		}
	}
	_, err := s.q.Exec(ctx, "DELETE FROM limits WHERE org_id = $1 AND scope = $2", org, subject)
	return err
}

// DeleteSessionCredentials drops the registry credentials of an ended session and refuses new ones for it.
func (s *Store) DeleteSessionCredentials(ctx context.Context, session uuid.UUID) error {
	return s.InTx(ctx, func(tx *Store) error {
		if _, err := tx.q.Exec(ctx, `
			INSERT INTO registry_revoked_sessions (session_id, expires_at) VALUES ($1, now() + make_interval(secs => $2))
			ON CONFLICT DO NOTHING`, session, revokedSessionRetention.Seconds()); err != nil {
			return err
		}
		_, err := tx.q.Exec(ctx, "DELETE FROM registry_credentials WHERE session_id = $1", session)
		return err
	})
}
