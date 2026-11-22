package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type SessionKind string

const (
	SessionBrowser SessionKind = "browser"
	SessionCLI     SessionKind = "cli"
)

type Session struct {
	ID, UserID                       uuid.UUID
	Kind                             SessionKind
	UserAgent, IP                    string
	CreatedAt, LastUsedAt, ExpiresAt time.Time
}

const (
	sessionColumns   = "id, user_id, kind, user_agent, ip, created_at, last_used_at, expires_at"
	lastUsedInterval = time.Minute
)

func scanSession(row pgx.Row) (Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.UserID, &s.Kind, &s.UserAgent, &s.IP, &s.CreatedAt, &s.LastUsedAt, &s.ExpiresAt)
	return s, err
}

func (s *Store) CreateSession(ctx context.Context, userID uuid.UUID, kind SessionKind, ttl time.Duration, userAgent, ip string) (string, Session, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", Session{}, err
	}
	token := "obs_" + base64.RawURLEncoding.EncodeToString(raw)
	sess, err := scanSession(s.q.QueryRow(ctx, `
		INSERT INTO sessions (user_id, kind, token_hash, user_agent, ip, expires_at)
		VALUES ($1, $2, $3, $4, $5, now() + make_interval(secs => $6))
		RETURNING `+sessionColumns, userID, kind, hashToken(token), userAgent, ip, ttl.Seconds()))
	if err != nil {
		return "", Session{}, err
	}
	return token, sess, nil
}

func (s *Store) SessionByToken(ctx context.Context, token string) (Session, error) {
	sess, err := scanSession(s.q.QueryRow(ctx, `
		SELECT `+sessionColumns+` FROM sessions
		WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now()`, hashToken(token)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	if time.Since(sess.LastUsedAt) > lastUsedInterval {
		err = s.q.QueryRow(ctx, "UPDATE sessions SET last_used_at = now() WHERE id = $1 RETURNING last_used_at", sess.ID).Scan(&sess.LastUsedAt)
		if err != nil {
			return Session{}, err
		}
	}
	return sess, nil
}

func (s *Store) Sessions(ctx context.Context, userID uuid.UUID) ([]Session, error) {
	rows, err := s.q.Query(ctx, `
		SELECT `+sessionColumns+` FROM sessions
		WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > now()
		ORDER BY created_at DESC, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

func (s *Store) RevokeSession(ctx context.Context, userID, sessionID uuid.UUID) error {
	tag, err := s.q.Exec(ctx, `
		UPDATE sessions SET revoked_at = now()
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL AND expires_at > now()`, sessionID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SessionState returns a session's user and expiry and whether it was revoked, also once it ended; ErrNotFound if it never existed.
func (s *Store) SessionState(ctx context.Context, id uuid.UUID) (user uuid.UUID, expires time.Time, revoked bool, err error) {
	err = s.q.QueryRow(ctx, "SELECT user_id, expires_at, revoked_at IS NOT NULL FROM sessions WHERE id = $1", id).Scan(&user, &expires, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return user, expires, revoked, err
}

// SessionActive reports whether a session is neither revoked nor expired; a primary key lookup.
func (s *Store) SessionActive(ctx context.Context, id uuid.UUID) (bool, error) {
	var ok bool
	err := s.q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM sessions WHERE id = $1 AND revoked_at IS NULL AND expires_at > now())`, id).Scan(&ok)
	return ok, err
}
