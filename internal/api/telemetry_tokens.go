package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/store"
)

const telemetryTokenRecheck = 60 * time.Second

// telemetryTokenRules are what a creator must hold, unblocked by any deny, to mint and keep an org-wide token.
var telemetryTokenRules = []authz.Rule{
	{Effect: authz.Allow, Object: authz.ObjSettings, Action: "telemetry:token"},
	{Effect: authz.Allow, Object: authz.ObjVMs + "/*", Action: "vm:metrics"},
	{Effect: authz.Allow, Object: authz.ObjNodes + "/*", Action: "node:metrics"},
}

type telemetryTokenJSON struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	CreatedBy  uuid.UUID  `json:"createdBy"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
}

func toTelemetryTokenJSON(t store.TelemetryToken) telemetryTokenJSON {
	return telemetryTokenJSON{t.ID, t.Name, t.CreatedBy, t.CreatedAt, t.LastUsedAt}
}

func telemetryTokenAudit(r *http.Request, action string, t store.TelemetryToken) store.AuditEntry {
	e := auditEntry(r, &t.OrgID, action, "telemetry-token/"+t.ID.String(), store.ResultAllowed)
	e.Details = map[string]any{"name": t.Name}
	return e
}

func (h *api) listTelemetryTokens(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.Org.ID, authz.ObjSettings, "telemetry:token") {
		return
	}
	ts, err := h.store.TelemetryTokens(r.Context(), m.Org.ID)
	if err != nil {
		serverError(w, "telemetry tokens", err)
		return
	}
	out := make([]telemetryTokenJSON, 0, len(ts))
	for _, t := range ts {
		out = append(out, toTelemetryTokenJSON(t))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *api) createTelemetryToken(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	name, ok := readName(w, r)
	if !ok || !h.authorize(w, r, m.Org.ID, authz.ObjSettings, "telemetry:token") {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	if err := h.z.Enforcer().CanHold(authz.UserSubject(p.User.ID), m.Org.ID, telemetryTokenRules); err != nil {
		h.auditDenied(r, m.Org.ID, authz.ObjSettings, store.ActionGrafanaTokenCreate)
		writeError(w, http.StatusForbidden, "creating a token requires org-wide vm:metrics and node:metrics without deny rules")
		return
	}
	var tok store.TelemetryToken
	var secret string
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if tok, secret, err = tx.CreateTelemetryToken(r.Context(), m.Org.ID, p.User.ID, name); err != nil {
			return err
		}
		return tx.Audit(r.Context(), telemetryTokenAudit(r, store.ActionGrafanaTokenCreate, tok))
	}) {
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		telemetryTokenJSON
		Token string `json:"token"`
	}{toTelemetryTokenJSON(tok), secret})
}

func (h *api) revokeTelemetryToken(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.Org.ID, authz.ObjSettings, "telemetry:token") {
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.RevokeTelemetryToken(r.Context(), m.Org.ID, id); err != nil {
			return err
		}
		return tx.Audit(r.Context(), telemetryTokenAudit(r, store.ActionGrafanaTokenRevoke, store.TelemetryToken{ID: id, OrgID: m.Org.ID}))
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// grafanaAuth accepts only an Authorization bearer token (never the session cookie) and revokes tokens whose creator lost access.
func (h *api) grafanaAuth(r *http.Request) (uuid.UUID, store.TelemetryToken, int) {
	scheme, secret, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	secret = strings.TrimSpace(secret)
	if !ok || !strings.EqualFold(scheme, "Bearer") || secret == "" {
		return uuid.Nil, store.TelemetryToken{}, http.StatusUnauthorized
	}
	ctx := r.Context()
	tok, err := h.store.TelemetryTokenBySecret(ctx, secret)
	if errors.Is(err, store.ErrNotFound) {
		return uuid.Nil, store.TelemetryToken{}, http.StatusUnauthorized
	}
	if err != nil {
		slog.Error("telemetry token", "err", err)
		return uuid.Nil, store.TelemetryToken{}, http.StatusInternalServerError
	}
	now := h.now()
	if checked, ok := h.tokenChecks.Load(tok.ID); !ok || now.Sub(checked.(time.Time)) >= telemetryTokenRecheck {
		holds, err := h.creatorHoldsToken(ctx, tok)
		if err != nil {
			slog.Error("telemetry token creator", "token", tok.ID, "err", err)
			return uuid.Nil, store.TelemetryToken{}, http.StatusInternalServerError
		}
		if !holds {
			h.tokenChecks.Delete(tok.ID)
			h.revokeLostToken(ctx, tok)
			return uuid.Nil, store.TelemetryToken{}, http.StatusUnauthorized
		}
		h.tokenChecks.Store(tok.ID, now)
	}
	if err := h.store.TouchTelemetryToken(ctx, tok.ID); err != nil {
		slog.Warn("touch telemetry token", "token", tok.ID, "err", err)
	}
	return tok.OrgID, tok, http.StatusOK
}

func (h *api) creatorHoldsToken(ctx context.Context, tok store.TelemetryToken) (bool, error) {
	if _, err := h.store.Membership(ctx, tok.OrgID, tok.CreatedBy); errors.Is(err, store.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return h.z.Enforcer().CanHold(authz.UserSubject(tok.CreatedBy), tok.OrgID, telemetryTokenRules) == nil, nil
}

func (h *api) revokeLostToken(ctx context.Context, tok store.TelemetryToken) {
	err := h.store.InTx(ctx, func(tx *store.Store) error {
		if err := tx.RevokeTelemetryToken(ctx, tok.OrgID, tok.ID); err != nil {
			return err
		}
		return tx.Audit(ctx, store.AuditEntry{ActorKind: store.ActorSystem, OrgID: &tok.OrgID, Action: store.ActionGrafanaTokenRevoke,
			Resource: "telemetry-token/" + tok.ID.String(), Result: store.ResultAllowed,
			Details: map[string]any{"name": tok.Name, "reason": "creator lost access"}})
	})
	// A concurrent request may have revoked it first.
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Error("revoke telemetry token", "token", tok.ID, "err", err)
	}
}
