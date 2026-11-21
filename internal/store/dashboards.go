package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const maxDashboards = 50

// Dashboard panels are stored as given; the API validates their shape.
type Dashboard struct {
	ID, OrgID            uuid.UUID
	Scope                EnvScope
	OwnerID              *uuid.UUID
	Name                 string
	Panels               json.RawMessage
	CreatedBy            uuid.UUID
	CreatedAt, UpdatedAt time.Time
}

const dashboardCols = "id, org_id, scope, coalesce(team_id, user_id), name, panels, created_by, created_at, updated_at"

func scanDashboard(r pgx.Row) (d Dashboard, err error) {
	err = r.Scan(&d.ID, &d.OrgID, &d.Scope, &d.OwnerID, &d.Name, &d.Panels, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return d, err
}

func dashboardOwner(d Dashboard) EnvVar {
	return EnvVar{OrgID: d.OrgID, Scope: d.Scope, OwnerID: d.OwnerID}
}

func dashboardPanels(d Dashboard) json.RawMessage {
	if len(d.Panels) == 0 {
		return json.RawMessage("[]")
	}
	return d.Panels
}

func (s *Store) CreateDashboard(ctx context.Context, d Dashboard) (Dashboard, error) {
	err := s.InTx(ctx, func(tx *Store) error {
		var lock uuid.UUID
		if err := tx.q.QueryRow(ctx, "SELECT id FROM orgs WHERE id = $1 FOR UPDATE", d.OrgID).Scan(&lock); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if err := tx.lockEnvOwner(ctx, dashboardOwner(d)); err != nil {
			return err
		}
		var n int
		if err := tx.q.QueryRow(ctx, "SELECT count(*) FROM dashboards WHERE org_id = $1", d.OrgID).Scan(&n); err != nil {
			return err
		}
		if n >= maxDashboards {
			return ErrLimitExceeded
		}
		teamID, userID := envOwnerColumns(dashboardOwner(d))
		var err error
		d, err = scanDashboard(tx.q.QueryRow(ctx, `
			INSERT INTO dashboards (org_id, scope, team_id, user_id, name, panels, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING `+dashboardCols, d.OrgID, d.Scope, teamID, userID, d.Name, dashboardPanels(d), d.CreatedBy))
		return err
	})
	return d, err
}

func (s *Store) Dashboard(ctx context.Context, org, id uuid.UUID) (Dashboard, error) {
	return scanDashboard(s.q.QueryRow(ctx, "SELECT "+dashboardCols+" FROM dashboards WHERE org_id = $1 AND id = $2", org, id))
}

func (s *Store) Dashboards(ctx context.Context, org uuid.UUID) ([]Dashboard, error) {
	rows, err := s.q.Query(ctx, "SELECT "+dashboardCols+" FROM dashboards WHERE org_id = $1 ORDER BY name, id", org)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Dashboard, error) { return scanDashboard(r) })
}

// UpdateDashboard saves name, scope, owner and panels; ErrConflict when the dashboard changed since expectUpdatedAt.
func (s *Store) UpdateDashboard(ctx context.Context, d Dashboard, expectUpdatedAt time.Time) (Dashboard, error) {
	err := s.InTx(ctx, func(tx *Store) error {
		if err := tx.lockEnvOwner(ctx, dashboardOwner(d)); err != nil {
			return err
		}
		teamID, userID := envOwnerColumns(dashboardOwner(d))
		// clock_timestamp, not now(): a transaction started earlier must not move updated_at backwards.
		updated, err := scanDashboard(tx.q.QueryRow(ctx, `
			UPDATE dashboards SET scope = $3, team_id = $4, user_id = $5, name = $6, panels = $7, updated_at = clock_timestamp()
			WHERE org_id = $1 AND id = $2 AND updated_at = $8 RETURNING `+dashboardCols,
			d.OrgID, d.ID, d.Scope, teamID, userID, d.Name, dashboardPanels(d), expectUpdatedAt))
		if errors.Is(err, ErrNotFound) {
			if _, err := tx.Dashboard(ctx, d.OrgID, d.ID); err != nil {
				return err
			}
			return ErrConflict
		}
		d = updated
		return err
	})
	return d, err
}

func (s *Store) DeleteDashboard(ctx context.Context, org, id uuid.UUID) error {
	tag, err := s.q.Exec(ctx, "DELETE FROM dashboards WHERE org_id = $1 AND id = $2", org, id)
	if err == nil && tag.RowsAffected() == 0 {
		err = ErrNotFound
	}
	return err
}
