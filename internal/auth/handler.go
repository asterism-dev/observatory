package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/asterism-dev/observatory/internal/store"
)

const (
	oidcCookie     = "obs_oidc"
	oidcCookieTTL  = 10 * time.Minute
	browserSessTTL = 7 * 24 * time.Hour
)

type Handler struct {
	Store       *store.Store
	Auth        Authenticator
	PublicURL   *url.URL
	Issuer      string
	CLIClientID string
}

type oidcState struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Redirect string `json:"r"`
}

func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", h.login)
	mux.HandleFunc("GET /auth/callback", h.callback)
	mux.Handle("POST /auth/logout", h.Require(http.HandlerFunc(h.logout)))
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	st := oidcState{State: randomString(), Verifier: oauth2.GenerateVerifier(), Redirect: safeRedirect(q.Get("redirect"))}
	raw, err := json.Marshal(st)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	http.SetCookie(w, h.cookie(oidcCookie, base64.RawURLEncoding.EncodeToString(raw), "/auth", oidcCookieTTL))
	http.Redirect(w, r, h.Auth.AuthCodeURL(st.State, st.Verifier, q.Get("register") == "1"), http.StatusFound)
}

func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	ip, ua := RequestMeta(r)
	http.SetCookie(w, h.cookie(oidcCookie, "", "/auth", -1))
	fail := func(reason string) {
		if err := h.Store.Audit(r.Context(), store.AuditEntry{
			ActorKind: store.ActorSystem, Action: store.ActionAuthLogin, Result: store.ResultFailed,
			IP: ip, UserAgent: ua, Details: map[string]any{"reason": reason},
		}); err != nil {
			slog.Error("audit failed login", "reason", reason, "ip", ip, "err", err)
		}
		http.Error(w, "login failed", http.StatusBadRequest)
	}

	st, ok := readOIDCState(r)
	q := r.URL.Query()
	if !ok || q.Get("state") == "" || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.State)) != 1 {
		fail("state mismatch")
		return
	}
	if e := q.Get("error"); e != "" {
		fail("idp error: " + e)
		return
	}
	id, err := h.Auth.Exchange(r.Context(), q.Get("code"), st.Verifier)
	if err != nil {
		fail("exchange: " + err.Error())
		return
	}

	token, err := h.startSession(r.Context(), id, ip, ua)
	if err != nil {
		slog.Error("start session", "subject", id.Subject, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, h.cookie(sessionCookie, token, "/", browserSessTTL))
	http.Redirect(w, r, safeRedirect(st.Redirect), http.StatusFound)
}

// startSession logs the identity in, creates a browser session and writes the audit entries in one transaction.
func (h *Handler) startSession(ctx context.Context, id store.Identity, ip, ua string) (string, error) {
	var token string
	err := h.Store.InTx(ctx, func(tx *store.Store) error {
		res, err := tx.Login(ctx, id)
		if err != nil {
			return err
		}
		var sess store.Session
		token, sess, err = tx.CreateSession(ctx, res.User.ID, store.SessionBrowser, browserSessTTL, ua, ip)
		if err != nil {
			return err
		}
		entry := func(action, resource string, orgID *uuid.UUID) store.AuditEntry {
			return store.AuditEntry{
				ActorKind: store.ActorUser, ActorID: res.User.ID.String(), OrgID: orgID,
				Action: action, Resource: resource, Result: store.ResultAllowed, IP: ip, UserAgent: ua,
			}
		}
		entries := []store.AuditEntry{entry(store.ActionAuthLogin, "session/"+sess.ID.String(), nil)}
		if res.Created && res.PersonalOrg != nil {
			entries = append(entries, entry(store.ActionOrgCreate, "org/"+res.PersonalOrg.ID.String(), &res.PersonalOrg.ID))
		}
		if res.BecamePlatformAdmin {
			entries = append(entries, entry(store.ActionPlatformGrantAdmin, "user/"+res.User.ID.String(), nil))
		}
		for _, e := range entries {
			if err := tx.Audit(ctx, e); err != nil {
				return err
			}
		}
		return nil
	})
	return token, err
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	ip, ua := RequestMeta(r)
	err := h.Store.InTx(r.Context(), func(tx *store.Store) error {
		if err := tx.RevokeSession(r.Context(), p.User.ID, p.Session.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		return tx.Audit(r.Context(), store.AuditEntry{
			ActorKind: store.ActorUser, ActorID: p.User.ID.String(), Action: store.ActionAuthLogout,
			Resource: "session/" + p.Session.ID.String(), Result: store.ResultAllowed, IP: ip, UserAgent: ua,
		})
	})
	if err != nil {
		slog.Error("logout", "session", p.Session.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	http.SetCookie(w, h.cookie(sessionCookie, "", "/", -1))
	w.WriteHeader(http.StatusNoContent)
}

// cookie builds an HttpOnly, SameSite=Lax cookie; a negative ttl deletes it.
func (h *Handler) cookie(name, value, path string, ttl time.Duration) *http.Cookie {
	maxAge := int(ttl.Seconds())
	if ttl < 0 {
		maxAge = -1
	}
	return &http.Cookie{
		Name: name, Value: value, Path: path, MaxAge: maxAge,
		HttpOnly: true, Secure: h.PublicURL.Scheme == "https", SameSite: http.SameSiteLaxMode,
	}
}

func readOIDCState(r *http.Request) (oidcState, bool) {
	c, err := r.Cookie(oidcCookie)
	if err != nil {
		return oidcState{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return oidcState{}, false
	}
	var st oidcState
	if err := json.Unmarshal(raw, &st); err != nil || st.State == "" || st.Verifier == "" {
		return oidcState{}, false
	}
	return st, true
}

// safeRedirect keeps only same-origin paths; browsers treat "//host" and "/\host" as off-site and strip tabs/newlines first.
func safeRedirect(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, `/\`) ||
		strings.ContainsFunc(p, func(c rune) bool { return c < 0x20 || c == 0x7f }) {
		return "/"
	}
	return p
}

func randomString() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error since Go 1.24
	return base64.RawURLEncoding.EncodeToString(b)
}
