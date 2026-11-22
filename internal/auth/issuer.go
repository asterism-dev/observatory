package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/asterism-dev/observatory/internal/platform/identity"
	"github.com/asterism-dev/observatory/internal/platform/servicetoken"
	"github.com/asterism-dev/observatory/internal/store"
)

const (
	signingKeySecret = "iam-signing-key"
	tokenCaller      = "edge"
)

// Issuer mints user JWTs for the edge until the iam service takes over.
type Issuer struct {
	store  *store.Store
	signer *identity.Signer
}

// NewIssuer loads the Ed25519 signing key from hub_secrets, creating it on first start.
func NewIssuer(ctx context.Context, s *store.Store, url string) (*Issuer, error) {
	seed, err := s.HubSecret(ctx, signingKeySecret, func() ([]byte, error) {
		seed := make([]byte, ed25519.SeedSize)
		_, err := rand.Read(seed)
		return seed, err
	})
	if err != nil {
		return nil, fmt.Errorf("hub secret %s: %w", signingKeySecret, err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("hub secret %s: want %d bytes, got %d", signingKeySecret, ed25519.SeedSize, len(seed))
	}
	return &Issuer{store: s, signer: identity.NewSigner(ed25519.NewKeyFromSeed(seed), url)}, nil
}

// Mint signs a JWT for u's session s and returns it with its expiry.
func (i *Issuer) Mint(u store.User, s store.Session) (string, time.Time, error) {
	return i.signer.Mint(identity.Principal{User: u.ID, Session: s.ID, PlatformAdmin: u.PlatformAdmin, Name: u.Name, SessionKind: string(s.Kind), SessionExpires: s.ExpiresAt})
}

// TokenHandler exchanges a session token (browser cookie or CLI bearer) for a short-lived JWT.
func (i *Issuer) TokenHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var in struct {
			SessionToken string `json:"session_token"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		caller, _ := servicetoken.Caller(r.Context())
		// Only the edge turns sessions into user identities; a service holding a session token must not impersonate its user.
		if caller != tokenCaller {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if in.SessionToken == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		sess, err := i.store.SessionByToken(r.Context(), in.SessionToken)
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err != nil {
			slog.Error("iam token: session lookup", "caller", caller, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		user, err := i.store.User(r.Context(), sess.UserID)
		if err != nil {
			slog.Error("iam token: load user", "caller", caller, "user", sess.UserID, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		signed, exp, err := i.Mint(user, sess)
		if err != nil {
			slog.Error("iam token: sign", "caller", caller, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"token": signed, "expires_at": exp})
	})
}

// JWKSHandler publishes the verification keys.
func (i *Issuer) JWKSHandler() http.Handler { return i.signer.JWKSHandler() }
