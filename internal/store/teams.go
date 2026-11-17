package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/asterism-dev/observatory/internal/authz"
)

type Team struct {
	ID, OrgID   uuid.UUID
	Name        string
	MemberCount int
	CreatedAt   time.Time
}

type TeamMember struct {
	User  User
	Admin bool
}

const teamColumns = `t.id, t.org_id, t.name, (SELECT count(*) FROM team_members tm WHERE tm.team_id = t.id), t.created_at`

func scanTeam(r pgx.Row) (t Team, err error) {
	err = r.Scan(&t.ID, &t.OrgID, &t.Name, &t.MemberCount, &t.CreatedAt)
	return
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (s *Store) CreateTeam(ctx context.Context, orgID uuid.UUID, name string) (Team, error) {
	t := Team{OrgID: orgID, Name: name}
	err := s.q.QueryRow(ctx, "INSERT INTO teams (org_id, name) VALUES ($1, $2) RETURNING id, created_at", orgID, name).Scan(&t.ID, &t.CreatedAt)
	if isUniqueViolation(err) {
		return Team{}, ErrConflict
	}
	return t, err
}

func (s *Store) RenameTeam(ctx context.Context, orgID, teamID uuid.UUID, name string) error {
	tag, err := s.q.Exec(ctx, "UPDATE teams SET name = $3 WHERE org_id = $1 AND id = $2", orgID, teamID, name)
	switch {
	case isUniqueViolation(err):
		return ErrConflict
	case err != nil:
		return err
	case tag.RowsAffected() == 0:
		return ErrNotFound
	}
	return nil
}

// DeleteTeam also drops the team's memberships (cascade) and the role bindings whose subject is the team.
func (s *Store) DeleteTeam(ctx context.Context, orgID, teamID uuid.UUID) error {
	return s.InTx(ctx, func(tx *Store) error {
		if err := tx.LockOrg(ctx, orgID); err != nil {
			return err
		}
		tag, err := tx.q.Exec(ctx, "DELETE FROM teams WHERE org_id = $1 AND id = $2", orgID, teamID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		_, err = tx.q.Exec(ctx, "DELETE FROM role_bindings WHERE org_id = $1 AND subject = $2", orgID, authz.TeamSubject(teamID))
		return err
	})
}

func (s *Store) Teams(ctx context.Context, orgID uuid.UUID) ([]Team, error) {
	rows, err := s.q.Query(ctx, "SELECT "+teamColumns+" FROM teams t WHERE t.org_id = $1 ORDER BY t.name, t.id", orgID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Team, error) { return scanTeam(r) })
}

func (s *Store) Team(ctx context.Context, orgID, teamID uuid.UUID) (Team, error) {
	t, err := scanTeam(s.q.QueryRow(ctx, "SELECT "+teamColumns+" FROM teams t WHERE t.org_id = $1 AND t.id = $2", orgID, teamID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Team{}, ErrNotFound
	}
	return t, err
}

func (s *Store) TeamMembers(ctx context.Context, orgID, teamID uuid.UUID) ([]TeamMember, error) {
	rows, err := s.q.Query(ctx, `
		SELECT u.id, u.subject, u.email, u.name, u.platform_admin, u.created_at, tm.admin
		FROM team_members tm
		JOIN teams t ON t.id = tm.team_id
		JOIN users u ON u.id = tm.user_id
		WHERE t.org_id = $1 AND t.id = $2
		ORDER BY u.name, u.id`, orgID, teamID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (m TeamMember, err error) {
		err = r.Scan(&m.User.ID, &m.User.Subject, &m.User.Email, &m.User.Name, &m.User.PlatformAdmin, &m.User.CreatedAt, &m.Admin)
		return
	})
}

func (s *Store) SetTeamMember(ctx context.Context, orgID, teamID, userID uuid.UUID, admin bool) error {
	return s.InTx(ctx, func(tx *Store) error {
		if err := tx.LockOrg(ctx, orgID); err != nil {
			return err
		}
		if _, err := tx.Team(ctx, orgID, teamID); err != nil {
			return err
		}
		// FOR SHARE conflicts with RemoveMember's row lock, so the user can't leave the org mid-insert.
		var one int
		err := tx.q.QueryRow(ctx, "SELECT 1 FROM memberships WHERE org_id = $1 AND user_id = $2 FOR SHARE", orgID, userID).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotMember
		} else if err != nil {
			return err
		}
		_, err = tx.q.Exec(ctx, `
			INSERT INTO team_members (team_id, user_id, admin) VALUES ($1, $2, $3)
			ON CONFLICT (team_id, user_id) DO UPDATE SET admin = EXCLUDED.admin`, teamID, userID, admin)
		return err
	})
}

func (s *Store) RemoveTeamMember(ctx context.Context, orgID, teamID, userID uuid.UUID) error {
	return s.InTx(ctx, func(tx *Store) error {
		if err := tx.LockOrg(ctx, orgID); err != nil {
			return err
		}
		tag, err := tx.q.Exec(ctx, `
			DELETE FROM team_members WHERE team_id = $2 AND user_id = $3
			AND team_id IN (SELECT id FROM teams WHERE org_id = $1)`, orgID, teamID, userID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// LockTeamMember locks the user's team_members row and returns its admin flag; ErrNotFound if the user isn't in the team. Take LockOrg first.
func (s *Store) LockTeamMember(ctx context.Context, orgID, teamID, userID uuid.UUID) (bool, error) {
	var admin bool
	err := s.q.QueryRow(ctx, `
		SELECT tm.admin FROM team_members tm JOIN teams t ON t.id = tm.team_id
		WHERE t.org_id = $1 AND t.id = $2 AND tm.user_id = $3 FOR UPDATE OF tm`, orgID, teamID, userID).Scan(&admin)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	return admin, err
}
