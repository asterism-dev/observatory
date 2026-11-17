package store

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/asterism-dev/observatory/internal/seal"
)

var ErrNoMasterKey = errors.New("no master key configured")

type EnvKind string

const (
	EnvPlain  EnvKind = "env"
	EnvSecret EnvKind = "secret"
)

type EnvScope string

const (
	EnvScopeOrg  EnvScope = "org"
	EnvScopeTeam EnvScope = "team"
	EnvScopeUser EnvScope = "user"
)

// OwnerID is the team or user id, nil for org scope.
type EnvVar struct {
	ID, OrgID            uuid.UUID
	Kind                 EnvKind
	Scope                EnvScope
	OwnerID              *uuid.UUID
	Name                 string
	CreatedBy, UpdatedBy uuid.UUID
	CreatedAt, UpdatedAt time.Time
}

const envCols = "id, org_id, kind, scope, coalesce(team_id, user_id), name, created_by, updated_by, created_at, updated_at"

func scanEnvVar(r pgx.Row) (v EnvVar, err error) {
	err = r.Scan(&v.ID, &v.OrgID, &v.Kind, &v.Scope, &v.OwnerID, &v.Name, &v.CreatedBy, &v.UpdatedBy, &v.CreatedAt, &v.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return v, err
}

func envOwnerColumns(v EnvVar) (teamID, userID *uuid.UUID) {
	switch v.Scope {
	case EnvScopeTeam:
		return v.OwnerID, nil
	case EnvScopeUser:
		return nil, v.OwnerID
	}
	return nil, nil
}

// orgKey returns the org's data key, creating it on first use.
func (s *Store) orgKey(ctx context.Context, org uuid.UUID) (seal.Key, error) {
	if s.master == nil {
		return seal.Key{}, ErrNoMasterKey
	}
	load := func() (seal.Key, error) {
		var nonce, wrapped []byte
		if err := s.q.QueryRow(ctx, "SELECT nonce, wrapped FROM org_keys WHERE org_id = $1", org).Scan(&nonce, &wrapped); err != nil {
			return seal.Key{}, err
		}
		raw, err := seal.Open(*s.master, nonce, wrapped, org[:])
		if err != nil {
			return seal.Key{}, err
		}
		if len(raw) != len(seal.Key{}) {
			return seal.Key{}, errors.New("org key has wrong length")
		}
		return seal.Key(raw), nil
	}
	k, err := load()
	if !errors.Is(err, pgx.ErrNoRows) {
		return k, err
	}
	var fresh seal.Key
	if _, err := rand.Read(fresh[:]); err != nil {
		return seal.Key{}, err
	}
	nonce, wrapped, err := seal.Seal(*s.master, fresh[:], org[:])
	if err != nil {
		return seal.Key{}, err
	}
	if _, err := s.q.Exec(ctx, `
		INSERT INTO org_keys (org_id, master_key_id, nonce, wrapped) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
		org, s.master.ID(), nonce, wrapped); err != nil {
		return seal.Key{}, err
	}
	return load()
}

func envAAD(org, id uuid.UUID) []byte { return append(org[:len(org):len(org)], id[:]...) }

func (s *Store) EnvVars(ctx context.Context, org uuid.UUID) ([]EnvVar, error) {
	rows, err := s.q.Query(ctx, "SELECT "+envCols+" FROM env_vars WHERE org_id = $1 ORDER BY name, id", org)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EnvVar, error) { return scanEnvVar(r) })
}

func (s *Store) EnvVar(ctx context.Context, org, id uuid.UUID) (EnvVar, error) {
	return scanEnvVar(s.q.QueryRow(ctx, "SELECT "+envCols+" FROM env_vars WHERE org_id = $1 AND id = $2", org, id))
}

func (s *Store) EnvValue(ctx context.Context, v EnvVar) (string, error) {
	key, err := s.orgKey(ctx, v.OrgID)
	if err != nil {
		return "", err
	}
	var nonce, ct []byte
	err = s.q.QueryRow(ctx, "SELECT nonce, ciphertext FROM env_vars WHERE org_id = $1 AND id = $2", v.OrgID, v.ID).Scan(&nonce, &ct)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	} else if err != nil {
		return "", err
	}
	plain, err := seal.Open(key, nonce, ct, envAAD(v.OrgID, v.ID))
	return string(plain), err
}

func (s *Store) CreateEnvVar(ctx context.Context, v EnvVar, value string) (EnvVar, error) {
	key, err := s.orgKey(ctx, v.OrgID)
	if err != nil {
		return EnvVar{}, err
	}
	v.ID = uuid.New()
	nonce, ct, err := seal.Seal(key, []byte(value), envAAD(v.OrgID, v.ID))
	if err != nil {
		return EnvVar{}, err
	}
	teamID, userID := envOwnerColumns(v)
	err = s.q.QueryRow(ctx, `
		INSERT INTO env_vars (id, org_id, kind, scope, team_id, user_id, name, nonce, ciphertext, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10) RETURNING created_at, updated_at`,
		v.ID, v.OrgID, v.Kind, v.Scope, teamID, userID, v.Name, nonce, ct, v.CreatedBy).Scan(&v.CreatedAt, &v.UpdatedAt)
	if isUniqueViolation(err) {
		return EnvVar{}, ErrConflict
	} else if err != nil {
		return EnvVar{}, err
	}
	v.UpdatedBy = v.CreatedBy
	return v, nil
}

// UpdateEnvVar sets name, kind and updated_by from v; a nil value keeps the stored ciphertext.
func (s *Store) UpdateEnvVar(ctx context.Context, v EnvVar, value *string) error {
	var nonce, ct []byte
	if value != nil {
		key, err := s.orgKey(ctx, v.OrgID)
		if err != nil {
			return err
		}
		if nonce, ct, err = seal.Seal(key, []byte(*value), envAAD(v.OrgID, v.ID)); err != nil {
			return err
		}
	}
	tag, err := s.q.Exec(ctx, `
		UPDATE env_vars SET name = $3, kind = $4, updated_by = $5, updated_at = now(),
		  nonce = coalesce($6, nonce), ciphertext = coalesce($7, ciphertext)
		WHERE org_id = $1 AND id = $2`, v.OrgID, v.ID, v.Name, v.Kind, v.UpdatedBy, nonce, ct)
	return envWriteResult(tag.RowsAffected(), err)
}

// MoveEnvVar sets scope and owner from v; the value is not re-encrypted since the AAD has no scope.
func (s *Store) MoveEnvVar(ctx context.Context, v EnvVar) error {
	teamID, userID := envOwnerColumns(v)
	tag, err := s.q.Exec(ctx, `
		UPDATE env_vars SET scope = $3, team_id = $4, user_id = $5, updated_by = $6, updated_at = now()
		WHERE org_id = $1 AND id = $2`, v.OrgID, v.ID, v.Scope, teamID, userID, v.UpdatedBy)
	return envWriteResult(tag.RowsAffected(), err)
}

func (s *Store) DeleteEnvVar(ctx context.Context, org, id uuid.UUID) error {
	tag, err := s.q.Exec(ctx, "DELETE FROM env_vars WHERE org_id = $1 AND id = $2", org, id)
	return envWriteResult(tag.RowsAffected(), err)
}

func envWriteResult(rows int64, err error) error {
	switch {
	case isUniqueViolation(err):
		return ErrConflict
	case err != nil:
		return err
	case rows == 0:
		return ErrNotFound
	}
	return nil
}
