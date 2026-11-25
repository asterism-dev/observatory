package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Account is a resource owner; OrgID is the zero uuid for the platform account.
type Account struct {
	ID       uuid.UUID
	OrgID    uuid.UUID
	TeamID   *uuid.UUID
	Name     string
	Platform bool
}

// Subscription is a formerly user-scoped integration that must stay with its owner.
type Subscription struct {
	ID, Account, Owner uuid.UUID
}

// AccountSnapshot maps accounts and the account of every addressable resource ("vm/<id>", "disk/<id>", …).
type AccountSnapshot struct {
	Accounts      []Account
	Resources     map[string]uuid.UUID
	Subscriptions []Subscription
}

func (a AccountSnapshot) Default(org uuid.UUID) (uuid.UUID, bool) {
	for _, acc := range a.Accounts {
		if acc.OrgID == org && acc.TeamID == nil && acc.Name == "default" && !acc.Platform {
			return acc.ID, true
		}
	}
	return uuid.Nil, false
}

func (a AccountSnapshot) OfTeam(team uuid.UUID) (uuid.UUID, bool) {
	for _, acc := range a.Accounts {
		if acc.TeamID != nil && *acc.TeamID == team {
			return acc.ID, true
		}
	}
	return uuid.Nil, false
}

func (a AccountSnapshot) InOrg(org uuid.UUID) []uuid.UUID {
	var ids []uuid.UUID
	for _, acc := range a.Accounts {
		if acc.OrgID == org && !acc.Platform {
			ids = append(ids, acc.ID)
		}
	}
	return ids
}

func (s *Store) AccountSnapshot(ctx context.Context) (AccountSnapshot, error) {
	pool, ok := s.q.(*pgxpool.Pool)
	if !ok {
		return readAccounts(ctx, s.q)
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return AccountSnapshot{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return readAccounts(ctx, tx)
}

func readAccounts(ctx context.Context, q querier) (AccountSnapshot, error) {
	snap := AccountSnapshot{Resources: map[string]uuid.UUID{}}
	rows, err := q.Query(ctx, `SELECT id, coalesce(org_id, '00000000-0000-0000-0000-000000000000'::uuid), team_id, name, platform FROM accounts`)
	if err != nil {
		return snap, err
	}
	snap.Accounts, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Account, error) {
		var a Account
		return a, r.Scan(&a.ID, &a.OrgID, &a.TeamID, &a.Name, &a.Platform)
	})
	if err != nil {
		return snap, err
	}
	for _, t := range []struct{ kind, table string }{
		{"vm", "vms"}, {"disk", "disks"}, {"node", "nodes"}, {"network", "networks"}, {"integration", "integrations"},
	} {
		rows, err := q.Query(ctx, "SELECT id, account_id FROM "+t.table+" WHERE account_id IS NOT NULL")
		if err != nil {
			return snap, err
		}
		var id, acc uuid.UUID
		if _, err := pgx.ForEachRow(rows, []any{&id, &acc}, func() error {
			snap.Resources[t.kind+"/"+id.String()] = acc
			return nil
		}); err != nil {
			return snap, err
		}
	}
	rows, err = q.Query(ctx, `SELECT id, account_id, user_id FROM integrations
		WHERE scope = 'user' AND kind IN ('claude', 'gcloud') AND account_id IS NOT NULL`)
	if err != nil {
		return snap, err
	}
	snap.Subscriptions, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Subscription, error) {
		var s Subscription
		return s, r.Scan(&s.ID, &s.Account, &s.Owner)
	})
	return snap, err
}
