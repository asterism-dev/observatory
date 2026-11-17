package store

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

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
	ActionAuthLogin           = "auth:login"
	ActionAuthCLILogin        = "auth:cli-login"
	ActionAuthLogout          = "auth:logout"
	ActionSessionRevoke       = "session:revoke"
	ActionOrgCreate           = "org:create"
	ActionPlatformGrantAdmin  = "platform:grant-admin"
	ActionAuditRead           = "audit:read"
	ActionOrgUpdate           = "org:update"
	ActionMemberInvite        = "member:invite"
	ActionInviteRevoke        = "invite:revoke"
	ActionInviteAccept        = "invite:accept"
	ActionMemberRemove        = "member:remove"
	ActionMemberLeave         = "member:leave"
	ActionMemberSetRole       = "member:set-role"
	ActionTeamCreate          = "team:create"
	ActionTeamUpdate          = "team:update"
	ActionTeamDelete          = "team:delete"
	ActionTeamMemberSet       = "team:member-set"
	ActionTeamMemberRemove    = "team:member-remove"
	ActionTeamLeave           = "team:leave"
	ActionRoleCreate          = "role:create"
	ActionRoleUpdate          = "role:update"
	ActionRoleDelete          = "role:delete"
	ActionRoleBind            = "role:bind"
	ActionRoleUnbind          = "role:unbind"
	ActionLimitSet            = "limit:set"
	ActionPlatformRevokeAdmin = "platform:revoke-admin"
	ActionAdminRead           = "admin:read"
	ActionEnvSet              = "env:set"
	ActionEnvMove             = "env:move"
	ActionEnvDelete           = "env:delete"
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
	maxTextBytes      = 512
)

// SanitizeText makes client-controlled text storable: valid UTF-8, no control characters (Postgres rejects NUL), at most 512 bytes.
func SanitizeText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	if len(s) > maxTextBytes {
		s = strings.ToValidUTF8(s[:maxTextBytes], "")
	}
	return s
}

func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	details := make(map[string]any, len(e.Details))
	for k, v := range e.Details {
		if str, ok := v.(string); ok {
			v = SanitizeText(str)
		}
		details[k] = v
	}
	e.Details = details
	e.UserAgent = SanitizeText(e.UserAgent)
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
