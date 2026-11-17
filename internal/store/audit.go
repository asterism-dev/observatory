package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type ActorKind string

const (
	ActorUser   ActorKind = "user"
	ActorVM     ActorKind = "vm"
	ActorSystem ActorKind = "system"
)

type Result string

const (
	ResultAllowed Result = "allowed"
	ResultDenied  Result = "denied"
	ResultFailed  Result = "failed"
)

const (
	ActionAuthLogin          = "auth:login"
	ActionAuthCLILogin       = "auth:cli-login"
	ActionAuthLogout         = "auth:logout"
	ActionSessionRevoke      = "session:revoke"
	ActionOrgCreate          = "org:create"
	ActionPlatformGrantAdmin = "platform:grant-admin"
	ActionAuditRead          = "audit:read"
)

type AuditEntry struct {
	ID        int64
	At        time.Time
	ActorKind ActorKind
	ActorID   string
	OrgID     *uuid.UUID
	Action    string
	Resource  string
	Result    Result
	IP        string
	UserAgent string
	Details   map[string]any
}

type AuditFilter struct {
	ActorID, Action string
	OrgID           *uuid.UUID
	From, To        time.Time
	Before          int64
	Limit           int
}

const (
	defaultAuditLimit = 50
	maxAuditLimit     = 200
)

func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	_, err := s.q.Exec(ctx, `
		INSERT INTO audit_log (actor_kind, actor_id, org_id, action, resource, result, ip, user_agent, details)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		e.ActorKind, e.ActorID, e.OrgID, e.Action, e.Resource, e.Result, e.IP, e.UserAgent, e.Details)
	return err
}

func (s *Store) AuditEntries(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	var (
		where []string
		args  []any
	)
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.ActorID != "" {
		add("actor_id = $%d", f.ActorID)
	}
	if f.Action != "" {
		add("action = $%d", f.Action)
	}
	if f.OrgID != nil {
		add("org_id = $%d", *f.OrgID)
	}
	if !f.From.IsZero() {
		add("at >= $%d", f.From)
	}
	if !f.To.IsZero() {
		add("at < $%d", f.To)
	}
	if f.Before > 0 {
		add("id < $%d", f.Before)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultAuditLimit
	}
	limit = min(limit, maxAuditLimit)
	args = append(args, limit)

	query := "SELECT id, at, actor_kind, actor_id, org_id, action, resource, result, ip, user_agent, details FROM audit_log"
	for i, w := range where {
		if i == 0 {
			query += " WHERE " + w
		} else {
			query += " AND " + w
		}
	}
	query += fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))

	rows, err := s.q.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.At, &e.ActorKind, &e.ActorID, &e.OrgID, &e.Action, &e.Resource, &e.Result, &e.IP, &e.UserAgent, &e.Details); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
