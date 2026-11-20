package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/asterism-dev/observatory/internal/integrations"
	"github.com/asterism-dev/observatory/internal/seal"
)

// IntegrationAccount is one Claude account of an integration; its tokens never leave the store this way.
type IntegrationAccount struct {
	ID, IntegrationID, OrgID  uuid.UUID
	Position                  int
	Active, ActivatedManually bool
	Account                   string // email, "" for setup tokens
	Source                    integrations.Source
	AccessExpiresAt           *time.Time
	Status, LastError         string
	Usage                     AccountUsage
	LimitedUntil              *time.Time
	CreatedBy, UpdatedBy      uuid.UUID
	CreatedAt, UpdatedAt      time.Time
}

type AccountUsage struct {
	FiveHourPct, WeekPct           *float64 // 0–100
	FiveHourResetsAt, WeekResetsAt *time.Time
	Source                         string // "poll" | "proxy" | ""
	At                             *time.Time
}

// ResolvedAccount carries a decrypted access token for a VM's desired state.
type ResolvedAccount struct {
	ID    uuid.UUID
	Token string
}

func (a ResolvedAccount) String() string             { return fmt.Sprintf("ResolvedAccount{%s}", a.ID) }
func (a ResolvedAccount) GoString() string           { return a.String() }
func (ResolvedAccount) MarshalJSON() ([]byte, error) { return []byte(`"redacted"`), nil }

const accountCols = `id, integration_id, org_id, position, active, activated_manually, account, source, access_expires_at, status, last_error,
	five_hour_pct, week_pct, five_hour_resets_at, week_resets_at, usage_source, usage_at, limited_until, created_by, updated_by, created_at, updated_at`

func scanAccount(r pgx.CollectableRow) (a IntegrationAccount, err error) {
	u := &a.Usage
	err = r.Scan(&a.ID, &a.IntegrationID, &a.OrgID, &a.Position, &a.Active, &a.ActivatedManually, &a.Account, &a.Source, &a.AccessExpiresAt,
		&a.Status, &a.LastError, &u.FiveHourPct, &u.WeekPct, &u.FiveHourResetsAt, &u.WeekResetsAt, &u.Source, &u.At, &a.LimitedUntil,
		&a.CreatedBy, &a.UpdatedBy, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

// IntegrationAccounts lists integration's accounts in position order.
func (s *Store) IntegrationAccounts(ctx context.Context, org, integration uuid.UUID) ([]IntegrationAccount, error) {
	rows, err := s.q.Query(ctx, "SELECT "+accountCols+" FROM integration_accounts WHERE org_id = $1 AND integration_id = $2 ORDER BY position", org, integration)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanAccount)
}

// OrgIntegrationAccounts lists the accounts of all of org's integrations by integration id, each in position order.
func (s *Store) OrgIntegrationAccounts(ctx context.Context, org uuid.UUID) (map[uuid.UUID][]IntegrationAccount, error) {
	rows, err := s.q.Query(ctx, "SELECT "+accountCols+" FROM integration_accounts WHERE org_id = $1 ORDER BY integration_id, position", org)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, scanAccount)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID][]IntegrationAccount{}
	for _, a := range list {
		out[a.IntegrationID] = append(out[a.IntegrationID], a)
	}
	return out, nil
}

// insertAccount seals sec under id, which the first account of an integration shares with it.
func (s *Store) insertAccount(ctx context.Context, key seal.Key, v Integration, id uuid.UUID, position int, active bool, sec IntegrationSecret) error {
	nonce, ct, rnonce, rct, err := sealIntegration(key, v.OrgID, id, sec)
	if err != nil {
		return err
	}
	_, err = s.q.Exec(ctx, `
		INSERT INTO integration_accounts (id, integration_id, org_id, position, active, account, source, nonce, ciphertext,
		  refresh_nonce, refresh_ciphertext, access_expires_at, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $13)`,
		id, v.ID, v.OrgID, position, active, v.Account, v.Source, nonce, ct, rnonce, rct, sec.AccessExpiresAt, v.CreatedBy)
	return err
}

// resolvedAccounts opens the active account first, then the other working ones in position order.
func (s *Store) resolvedAccounts(ctx context.Context, key seal.Key, org, integration uuid.UUID) ([]ResolvedAccount, error) {
	rows, err := s.q.Query(ctx, `SELECT id, nonce, ciphertext FROM integration_accounts
		WHERE org_id = $1 AND integration_id = $2 AND (active OR status = 'ok') ORDER BY active DESC, position`, org, integration)
	if err != nil {
		return nil, err
	}
	type sealed struct {
		ID        uuid.UUID
		Nonce, CT []byte
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[sealed])
	if err != nil {
		return nil, err
	}
	out := make([]ResolvedAccount, 0, len(list))
	for _, a := range list {
		plain, err := seal.Open(key, a.Nonce, a.CT, integrationAAD(org, a.ID, "access"))
		if err != nil {
			return nil, fmt.Errorf("decrypt integration account %s: %w", a.ID, err)
		}
		out = append(out, ResolvedAccount{ID: a.ID, Token: string(plain)})
	}
	return out, nil
}
