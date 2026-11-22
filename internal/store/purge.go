package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
)

var scopedTables = []string{"image_defaults", "images", "env_vars", "integrations", "dashboards"}

// PurgeMember revokes the telemetry tokens user created in org on a policy up to removedAt, iam's last revision that can still hold the membership, then deletes their hub rows unless member, asked under the hub org lock, says they are back; running it again is harmless.
func (s *Store) PurgeMember(ctx context.Context, org, user uuid.UUID, removedAt uint64, member func(context.Context) (bool, error)) (purged bool, err error) {
	err = s.InTx(ctx, func(tx *Store) error {
		// User-scope creates take this lock too: one that saw the re-join committed before, one after waits and keeps its row.
		if err := tx.LockHubOrg(ctx, org); err != nil {
			return err
		}
		// Re-joining does not revive them, as in 0.x where removal revoked them at once.
		toks, err := tx.revokeTokensBy(ctx, org, user, removedAt)
		if err != nil {
			return err
		}
		for _, t := range toks {
			if err := tx.Audit(ctx, AuditEntry{ActorKind: ActorSystem, OrgID: &t.OrgID, Action: ActionGrafanaTokenRevoke, Resource: "telemetry-token/" + t.ID.String(),
				Result: ResultAllowed, Details: map[string]any{"name": t.Name, "reason": "creator left the organization"}}); err != nil {
				return err
			}
		}
		// ponytail: the transaction holds a connection and the org lock during the iam call; fine at today's purge rate.
		back, err := member(ctx)
		if err != nil || back {
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

// DeleteSessionCredentials drops the registry credentials of an ended session; their use is refused already, since iam reports the session ended.
func (s *Store) DeleteSessionCredentials(ctx context.Context, session uuid.UUID) error {
	_, err := s.q.Exec(ctx, "DELETE FROM registry_credentials WHERE session_id = $1", session)
	return err
}
