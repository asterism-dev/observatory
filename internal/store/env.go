package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/seal"
)

var (
	ErrNoMasterKey       = errors.New("no master key configured")
	ErrMasterKeyMismatch = errors.New("org keys are wrapped by a different master key than OBSERVATORY_SECRETS_KEY")
)

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
		var masterID string
		var nonce, wrapped []byte
		if err := s.q.QueryRow(ctx, "SELECT master_key_id, nonce, wrapped FROM org_keys WHERE org_id = $1", org).Scan(&masterID, &nonce, &wrapped); err != nil {
			return seal.Key{}, err
		}
		if masterID != s.master.ID() {
			return seal.Key{}, ErrMasterKeyMismatch
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

// CheckMasterKey fails if any org key was wrapped by another master key, so a wrong key can't split the hierarchy.
func (s *Store) CheckMasterKey(ctx context.Context) error {
	if s.master == nil {
		return ErrNoMasterKey
	}
	var other bool
	if err := s.q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM org_keys WHERE master_key_id <> $1) OR EXISTS (SELECT 1 FROM hub_secrets WHERE key_id <> $1)", s.master.ID()).Scan(&other); err != nil {
		return err
	}
	if other {
		return ErrMasterKeyMismatch
	}
	return nil
}

// envAAD binds the kind too, so flipping a secret to env in the database can't make the hub reveal it.
func envAAD(org, id uuid.UUID, kind EnvKind) []byte {
	return append(append(org[:len(org):len(org)], id[:]...), kind...)
}

// lockEnvOwner keeps a private variable's owner from being removed from the org before the write commits.
func (s *Store) lockEnvOwner(ctx context.Context, v EnvVar) error {
	if v.Scope != EnvScopeUser {
		return nil
	}
	var one int
	err := s.q.QueryRow(ctx, "SELECT 1 FROM memberships WHERE org_id = $1 AND user_id = $2 FOR SHARE", v.OrgID, v.OwnerID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

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
	plain, err := seal.Open(key, nonce, ct, envAAD(v.OrgID, v.ID, v.Kind))
	return string(plain), err
}

func (s *Store) CreateEnvVar(ctx context.Context, v EnvVar, value string) (EnvVar, error) {
	err := s.InTx(ctx, func(tx *Store) error {
		if err := tx.lockEnvOwner(ctx, v); err != nil {
			return err
		}
		key, err := tx.orgKey(ctx, v.OrgID)
		if err != nil {
			return err
		}
		v.ID = uuid.New()
		nonce, ct, err := seal.Seal(key, []byte(value), envAAD(v.OrgID, v.ID, v.Kind))
		if err != nil {
			return err
		}
		teamID, userID := envOwnerColumns(v)
		err = tx.q.QueryRow(ctx, `
			INSERT INTO env_vars (id, org_id, kind, scope, team_id, user_id, name, nonce, ciphertext, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10) RETURNING created_at, updated_at`,
			v.ID, v.OrgID, v.Kind, v.Scope, teamID, userID, v.Name, nonce, ct, v.CreatedBy).Scan(&v.CreatedAt, &v.UpdatedAt)
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	})
	if err != nil {
		return EnvVar{}, err
	}
	v.UpdatedBy = v.CreatedBy
	return v, nil
}

// ErrStale means the variable changed since the caller loaded and authorized it.
var ErrStale = errors.New("changed concurrently, reload")

// envGuard matches a row only while it still has the scope, owner and kind of old, the state the caller checked.
const envGuard = "org_id = $1 AND id = $2 AND scope = $3 AND team_id IS NOT DISTINCT FROM $4 AND user_id IS NOT DISTINCT FROM $5 AND kind = $6"

func envGuardArgs(old EnvVar, rest ...any) []any {
	teamID, userID := envOwnerColumns(old)
	return append([]any{old.OrgID, old.ID, old.Scope, teamID, userID, old.Kind}, rest...)
}

// UpdateEnvVar sets name, kind and updated_by from v if the row still matches old; a nil value keeps the stored value.
func (s *Store) UpdateEnvVar(ctx context.Context, old, v EnvVar, value *string) error {
	return s.InTx(ctx, func(tx *Store) error {
		if value == nil && v.Kind != old.Kind {
			// The kind is in the AAD, so the kept value is re-sealed; the lock keeps it from changing meanwhile.
			var nonce, ct []byte
			err := tx.q.QueryRow(ctx, "SELECT nonce, ciphertext FROM env_vars WHERE "+envGuard+" FOR UPDATE", envGuardArgs(old)...).Scan(&nonce, &ct)
			if errors.Is(err, pgx.ErrNoRows) {
				return tx.envWriteResult(ctx, old, 0, nil)
			} else if err != nil {
				return err
			}
			key, err := tx.orgKey(ctx, old.OrgID)
			if err != nil {
				return err
			}
			plain, err := seal.Open(key, nonce, ct, envAAD(old.OrgID, old.ID, old.Kind))
			if err != nil {
				return err
			}
			kept := string(plain)
			value = &kept
		}
		var nonce, ct []byte
		if value != nil {
			key, err := tx.orgKey(ctx, old.OrgID)
			if err != nil {
				return err
			}
			if nonce, ct, err = seal.Seal(key, []byte(*value), envAAD(old.OrgID, old.ID, v.Kind)); err != nil {
				return err
			}
		}
		tag, err := tx.q.Exec(ctx, `
			UPDATE env_vars SET name = $7, kind = $8, updated_by = $9, updated_at = now(),
			  nonce = coalesce($10, nonce), ciphertext = coalesce($11, ciphertext)
			WHERE `+envGuard, envGuardArgs(old, v.Name, v.Kind, v.UpdatedBy, nonce, ct)...)
		return tx.envWriteResult(ctx, old, tag.RowsAffected(), err)
	})
}

// MoveEnvVar sets scope and owner from v if the row still matches old; the value is not re-encrypted since the AAD has no scope.
func (s *Store) MoveEnvVar(ctx context.Context, old, v EnvVar) error {
	return s.InTx(ctx, func(tx *Store) error {
		if err := tx.lockEnvOwner(ctx, v); err != nil {
			return err
		}
		teamID, userID := envOwnerColumns(v)
		tag, err := tx.q.Exec(ctx, `
			UPDATE env_vars SET scope = $7, team_id = $8, user_id = $9, updated_by = $10, updated_at = now()
			WHERE `+envGuard, envGuardArgs(old, v.Scope, teamID, userID, v.UpdatedBy)...)
		return tx.envWriteResult(ctx, old, tag.RowsAffected(), err)
	})
}

// DeleteEnvVar deletes old if the row still matches it.
func (s *Store) DeleteEnvVar(ctx context.Context, old EnvVar) error {
	tag, err := s.q.Exec(ctx, "DELETE FROM env_vars WHERE "+envGuard, envGuardArgs(old)...)
	return s.envWriteResult(ctx, old, tag.RowsAffected(), err)
}

func (s *Store) envWriteResult(ctx context.Context, old EnvVar, rows int64, err error) error {
	switch {
	case isUniqueViolation(err):
		return ErrConflict
	case err != nil:
		return err
	case rows > 0:
		return nil
	}
	var exists bool
	if err := s.q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM env_vars WHERE org_id = $1 AND id = $2)", old.OrgID, old.ID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrStale
	}
	return ErrNotFound
}

// ResolveEnv returns the decrypted variables owner (a user or team subject) may use; the narrowest scope wins per name.
func (s *Store) ResolveEnv(ctx context.Context, e *authz.Enforcer, org uuid.UUID, owner string) (map[string]string, error) {
	scope, idStr, ok := strings.Cut(owner, ":")
	id, idErr := uuid.Parse(idStr)
	if !ok || idErr != nil || (scope != "user" && scope != "team") {
		return nil, fmt.Errorf("unsupported env owner %q", owner)
	}
	ownScope, ownObj := EnvScopeUser, authz.UserEnvObject(id)
	if scope == "team" {
		ownScope, ownObj = EnvScopeTeam, authz.TeamEnvObject(id)
	}
	orgOK := e.Allowed(owner, org, authz.ObjEnv, "env:use")
	ownOK := e.Allowed(owner, org, ownObj, "env:use")
	if !orgOK && !ownOK {
		return map[string]string{}, nil
	}
	rows, err := s.q.Query(ctx, `
		SELECT id, kind, scope, name, nonce, ciphertext FROM env_vars
		WHERE org_id = $1 AND (scope = 'org' OR (scope = $2 AND coalesce(team_id, user_id) = $3))`, org, ownScope, id)
	if err != nil {
		return nil, err
	}
	type row struct {
		id        uuid.UUID
		kind      EnvKind
		scope     EnvScope
		name      string
		nonce, ct []byte
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (x row, err error) {
		err = r.Scan(&x.id, &x.kind, &x.scope, &x.name, &x.nonce, &x.ct)
		return
	})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if len(list) == 0 {
		return out, nil
	}
	key, err := s.orgKey(ctx, org)
	if err != nil {
		return nil, err
	}
	for _, pass := range []EnvScope{EnvScopeOrg, ownScope} {
		if (pass == EnvScopeOrg && !orgOK) || (pass == ownScope && !ownOK) {
			continue
		}
		for _, r := range list {
			if r.scope != pass {
				continue
			}
			plain, err := seal.Open(key, r.nonce, r.ct, envAAD(org, r.id, r.kind))
			if err != nil {
				return nil, fmt.Errorf("decrypt env var %s: %w", r.id, err)
			}
			out[r.name] = string(plain)
		}
	}
	return out, nil
}
