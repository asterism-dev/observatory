package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

var (
	ErrMultiAccountNotAllowed = errors.New("several accounts need a private integration in proxy mode")
	ErrInvalid                = errors.New("invalid input")
)

// lockIntegration serializes account changes of one integration.
func (s *Store) lockIntegration(ctx context.Context, org, id uuid.UUID) error {
	var locked uuid.UUID
	err := s.q.QueryRow(ctx, "SELECT id FROM integrations WHERE org_id = $1 AND id = $2 FOR UPDATE", org, id).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// nextEligibleAccount picks the lowest-position working account of the integration that is not limited, ignoring exclude.
func nextEligibleAccount(ctx context.Context, tx *Store, integrationID, exclude uuid.UUID, now time.Time) (uuid.UUID, bool, error) {
	var id uuid.UUID
	err := tx.q.QueryRow(ctx, `SELECT id FROM integration_accounts
		WHERE integration_id = $1 AND id <> $2 AND status = 'ok' AND (limited_until IS NULL OR limited_until <= $3)
		ORDER BY position LIMIT 1`, integrationID, exclude, now).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	return id, err == nil, err
}

// AddIntegrationAccount appends an inactive account to a private Claude integration in proxy mode.
func (s *Store) AddIntegrationAccount(ctx context.Context, v Integration, a IntegrationAccount, sec IntegrationSecret) (IntegrationAccount, error) {
	sec, err := checkIntegrationSecret(a.Source, sec)
	if err != nil {
		return IntegrationAccount{}, err
	}
	id := uuid.New()
	err = s.InTx(ctx, func(tx *Store) error {
		if err := tx.lockIntegration(ctx, v.OrgID, v.ID); err != nil {
			return err
		}
		key, err := tx.orgKey(ctx, v.OrgID)
		if err != nil {
			return err
		}
		var scope EnvScope
		var mode integrations.Mode
		var kind integrations.Kind
		var position int
		if err := tx.q.QueryRow(ctx, `SELECT i.scope, i.mode, i.kind, coalesce((SELECT max(position) + 1 FROM integration_accounts WHERE integration_id = i.id), 0)
			FROM integrations i WHERE i.id = $1`, v.ID).Scan(&scope, &mode, &kind, &position); err != nil {
			return err
		}
		if kind != integrations.Claude || scope != EnvScopeUser || mode != integrations.ModeProxy {
			return ErrMultiAccountNotAllowed
		}
		v.Source, v.Account, v.CreatedBy = a.Source, a.Account, a.CreatedBy
		v.AccessExpiresAt = sec.AccessExpiresAt
		err = tx.insertAccount(ctx, key, v, id, position, false, sec)
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	})
	if err != nil {
		return IntegrationAccount{}, err
	}
	list, err := s.IntegrationAccounts(ctx, v.OrgID, v.ID)
	if err != nil {
		return IntegrationAccount{}, err
	}
	if i := slices.IndexFunc(list, func(x IntegrationAccount) bool { return x.ID == id }); i >= 0 {
		return list[i], nil
	}
	return IntegrationAccount{}, ErrNotFound
}

// ReplaceAccountToken stores a new verified token for a and marks it working again.
func (s *Store) ReplaceAccountToken(ctx context.Context, a IntegrationAccount, src integrations.Source, email string, sec IntegrationSecret, by uuid.UUID) error {
	sec, err := checkIntegrationSecret(src, sec)
	if err != nil {
		return err
	}
	key, err := s.orgKey(ctx, a.OrgID)
	if err != nil {
		return err
	}
	nonce, ct, rnonce, rct, err := sealIntegration(key, a.OrgID, a.ID, sec)
	if err != nil {
		return err
	}
	tag, err := s.q.Exec(ctx, `
		UPDATE integration_accounts SET source = $3, account = $4, nonce = $5, ciphertext = $6, refresh_nonce = $7, refresh_ciphertext = $8,
		  access_expires_at = $9, status = 'ok', last_error = '', updated_by = $10, updated_at = now()
		WHERE org_id = $1 AND id = $2`, a.OrgID, a.ID, src, email, nonce, ct, rnonce, rct, sec.AccessExpiresAt, by)
	return s.guardedWriteResult(ctx, "integration_accounts", a.OrgID, a.ID, tag.RowsAffected(), err)
}

// activate swaps the active flag to account id; callers hold the integration lock.
func (s *Store) activate(ctx context.Context, org, integration, id uuid.UUID, manual bool, by uuid.UUID) error {
	if _, err := s.q.Exec(ctx, "UPDATE integration_accounts SET active = false WHERE integration_id = $1 AND active AND id <> $2", integration, id); err != nil {
		return err
	}
	// by is uuid.Nil for an automatic switch, which keeps the last human editor.
	tag, err := s.q.Exec(ctx, `UPDATE integration_accounts SET active = true, activated_manually = $4,
		  updated_by = coalesce(nullif($5::uuid, '00000000-0000-0000-0000-000000000000'), updated_by), updated_at = now()
		WHERE org_id = $1 AND integration_id = $2 AND id = $3`, org, integration, id, manual, by)
	return s.guardedWriteResult(ctx, "integration_accounts", org, id, tag.RowsAffected(), err)
}

// ActivateAccount makes a the account VMs use; manual keeps the automatic switch from overriding the choice.
func (s *Store) ActivateAccount(ctx context.Context, a IntegrationAccount, manual bool, by uuid.UUID) error {
	return s.InTx(ctx, func(tx *Store) error {
		if err := tx.lockIntegration(ctx, a.OrgID, a.IntegrationID); err != nil {
			return err
		}
		if err := tx.activate(ctx, a.OrgID, a.IntegrationID, a.ID, manual, by); err != nil || !manual {
			return err
		}
		_, err := tx.q.Exec(ctx, "UPDATE integrations SET last_switch_at = NULL, last_switch_from = NULL, last_switch_to = NULL WHERE id = $1", a.IntegrationID)
		return err
	})
}

// ReorderAccounts sets the positions of v's accounts to the order of ids, which must name each exactly once.
func (s *Store) ReorderAccounts(ctx context.Context, v Integration, ids []uuid.UUID, by uuid.UUID) error {
	return s.InTx(ctx, func(tx *Store) error {
		if err := tx.lockIntegration(ctx, v.OrgID, v.ID); err != nil {
			return err
		}
		rows, err := tx.q.Query(ctx, "SELECT id FROM integration_accounts WHERE integration_id = $1", v.ID)
		if err != nil {
			return err
		}
		have, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		want := slices.Clone(ids)
		less := func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) }
		slices.SortFunc(have, less)
		slices.SortFunc(want, less)
		if !slices.Equal(have, want) {
			return ErrInvalid
		}
		_, err = tx.q.Exec(ctx, `UPDATE integration_accounts a SET position = o.pos - 1, updated_by = $2, updated_at = now()
			FROM unnest($1::uuid[]) WITH ORDINALITY AS o(id, pos) WHERE a.id = o.id`, ids, by)
		return err
	})
}

// DeleteIntegrationAccount removes a; when it was active the next eligible account takes over, or the first remaining one if all are limited or broken.
func (s *Store) DeleteIntegrationAccount(ctx context.Context, a IntegrationAccount) (last bool, err error) {
	err = s.InTx(ctx, func(tx *Store) error {
		if err := tx.lockIntegration(ctx, a.OrgID, a.IntegrationID); err != nil {
			return err
		}
		var wasActive bool
		err := tx.q.QueryRow(ctx, "DELETE FROM integration_accounts WHERE org_id = $1 AND integration_id = $2 AND id = $3 RETURNING active",
			a.OrgID, a.IntegrationID, a.ID).Scan(&wasActive)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var remaining int
		var first uuid.UUID
		if err := tx.q.QueryRow(ctx, "SELECT count(*), coalesce((SELECT id FROM integration_accounts WHERE integration_id = $1 ORDER BY position LIMIT 1), $2) FROM integration_accounts WHERE integration_id = $1",
			a.IntegrationID, uuid.Nil).Scan(&remaining, &first); err != nil {
			return err
		}
		if remaining == 0 {
			last = true
			_, err := tx.q.Exec(ctx, "DELETE FROM integrations WHERE org_id = $1 AND id = $2", a.OrgID, a.IntegrationID)
			return err
		}
		if !wasActive {
			return nil
		}
		next, ok, err := nextEligibleAccount(ctx, tx, a.IntegrationID, a.ID, time.Now())
		if err != nil {
			return err
		}
		if !ok {
			next = first
		}
		return tx.activate(ctx, a.OrgID, a.IntegrationID, next, false, a.UpdatedBy)
	})
	return last, err
}

// UsageObservation is one usage report for an account, from polling or the egress proxy.
type UsageObservation struct {
	Account                        uuid.UUID
	FiveHourPct, WeekPct           *float64 // 0–100
	FiveHourResetsAt, WeekResetsAt *time.Time
	LimitedUntil                   *time.Time // set only for a confirmed limit or a 100 % window with a future reset
	Source                         string     // "poll" | "proxy"
	ObservedAt                     time.Time
}

// Switch is an automatic change of an integration's active account.
type Switch struct {
	OrgID, IntegrationID, From, To uuid.UUID
	LimitedUntil                   time.Time
}

// autoSwitch moves the active account from to the next eligible one and records the switch; callers hold the integration lock.
func (s *Store) autoSwitch(ctx context.Context, org, integration, from uuid.UUID, until, now time.Time) (*Switch, error) {
	to, ok, err := nextEligibleAccount(ctx, s, integration, from, now)
	if err != nil || !ok {
		return nil, err
	}
	if err := s.activate(ctx, org, integration, to, false, uuid.Nil); err != nil {
		return nil, err
	}
	if _, err := s.q.Exec(ctx, "UPDATE integrations SET last_switch_at = $2, last_switch_from = $3, last_switch_to = $4 WHERE id = $1",
		integration, now, from, to); err != nil {
		return nil, err
	}
	return &Switch{OrgID: org, IntegrationID: integration, From: from, To: to, LimitedUntil: until}, nil
}

// RecordUsage stores obs's usage if newer than the stored usage_at and always applies its limit; when it limits the active, automatically activated account, it activates the next eligible one in the same tx.
func (s *Store) RecordUsage(ctx context.Context, obs UsageObservation, now time.Time) (*Switch, error) {
	var org, integration uuid.UUID
	err := s.q.QueryRow(ctx, "SELECT org_id, integration_id FROM integration_accounts WHERE id = $1", obs.Account).Scan(&org, &integration)
	// Reports can arrive after the account was removed.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var sw *Switch
	err = s.InTx(ctx, func(tx *Store) error {
		if err := tx.lockIntegration(ctx, org, integration); err != nil {
			return err
		}
		var active, manual bool
		var limitedUntil, usageAt *time.Time
		if err := tx.q.QueryRow(ctx, "SELECT active, activated_manually, limited_until, usage_at FROM integration_accounts WHERE id = $1 FOR UPDATE",
			obs.Account).Scan(&active, &manual, &limitedUntil, &usageAt); err != nil {
			return err
		}
		// Poll and proxy stamp ObservedAt on different clocks, so only usage follows the newest report; a limit always applies and only grows.
		if usageAt == nil || obs.ObservedAt.After(*usageAt) {
			if _, err := tx.q.Exec(ctx, `UPDATE integration_accounts SET five_hour_pct = $2, week_pct = $3, five_hour_resets_at = $4, week_resets_at = $5,
				usage_source = $6, usage_at = $7 WHERE id = $1`,
				obs.Account, obs.FiveHourPct, obs.WeekPct, obs.FiveHourResetsAt, obs.WeekResetsAt, obs.Source, obs.ObservedAt); err != nil {
				return err
			}
		}
		if obs.LimitedUntil != nil {
			if _, err := tx.q.Exec(ctx, "UPDATE integration_accounts SET limited_until = greatest(limited_until, $2) WHERE id = $1",
				obs.Account, obs.LimitedUntil); err != nil {
				return err
			}
		}
		wasLimited := limitedUntil != nil && limitedUntil.After(now)
		if !active || manual || wasLimited || obs.LimitedUntil == nil || !obs.LimitedUntil.After(now) {
			return nil
		}
		var err error
		sw, err = tx.autoSwitch(ctx, org, integration, obs.Account, *obs.LimitedUntil, now)
		return err
	})
	if errors.Is(err, ErrNotFound) || errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return sw, err
}

// ResumeLimited switches integrations whose active (non-manual) account is still limited to the first account whose limited_until has passed.
func (s *Store) ResumeLimited(ctx context.Context, now time.Time) ([]Switch, error) {
	rows, err := s.q.Query(ctx, `SELECT org_id, integration_id FROM integration_accounts a
		WHERE active AND NOT activated_manually AND limited_until > $1
		  AND EXISTS (SELECT 1 FROM integration_accounts o WHERE o.integration_id = a.integration_id AND o.id <> a.id
		    AND o.status = 'ok' AND (o.limited_until IS NULL OR o.limited_until <= $1))`, now)
	if err != nil {
		return nil, err
	}
	type candidate struct{ Org, Integration uuid.UUID }
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[candidate])
	if err != nil {
		return nil, err
	}
	var out []Switch
	for _, c := range list {
		var sw *Switch
		err := s.InTx(ctx, func(tx *Store) error {
			if err := tx.lockIntegration(ctx, c.Org, c.Integration); err != nil {
				return err
			}
			// Re-check under the lock: a manual activation or another switch may have run since the scan.
			var from uuid.UUID
			var until time.Time
			err := tx.q.QueryRow(ctx, `SELECT id, limited_until FROM integration_accounts
				WHERE integration_id = $1 AND active AND NOT activated_manually AND limited_until > $2`, c.Integration, now).Scan(&from, &until)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			} else if err != nil {
				return err
			}
			sw, err = tx.autoSwitch(ctx, c.Org, c.Integration, from, until, now)
			return err
		})
		if errors.Is(err, ErrNotFound) {
			continue
		} else if err != nil {
			return out, err
		}
		if sw != nil {
			out = append(out, *sw)
		}
	}
	return out, nil
}
