package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/asterism-dev/observatory/internal/platform/servicetoken"
	"github.com/asterism-dev/observatory/internal/platform/textx"
	"github.com/asterism-dev/observatory/internal/store"
)

const (
	signingKeySecret = "iam-signing-key"
	jwtAudience      = "observatory"
	jwtTTL           = 5 * time.Minute
	tokenCaller      = "edge"
)

// Issuer mints user JWTs for the edge until the iam service takes over.
type Issuer struct {
	store *store.Store
	url   string
	key   ed25519.PrivateKey
	kid   string
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
	key := ed25519.NewKeyFromSeed(seed)
	return &Issuer{store: s, url: url, key: key, kid: thumbprint(key.Public().(ed25519.PublicKey))}, nil
}

// thumbprint is the RFC 7638 JWK thumbprint, so the kid is stable for a key and changes with it.
func thumbprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256([]byte(`{"crv":"Ed25519","kty":"OKP","x":"` + base64.RawURLEncoding.EncodeToString(pub) + `"}`))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

type userClaims struct {
	jwt.RegisteredClaims
	SID  string `json:"sid"`
	PA   bool   `json:"pa"`
	Name string `json:"name"`
	SK   string `json:"sk"`
	SX   int64  `json:"sx"`
}

func (i *Issuer) claims(u store.User, s store.Session, now time.Time) userClaims {
	now = now.Truncate(time.Second)
	exp := now.Add(jwtTTL)
	// A token must not outlive its session; truncation keeps exp at or before the session's expiry.
	if end := s.ExpiresAt.Truncate(time.Second); end.Before(exp) {
		exp = end
	}
	return userClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    i.url,
			Audience:  jwt.ClaimStrings{jwtAudience},
			Subject:   u.ID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
		SID:  s.ID.String(),
		PA:   u.PlatformAdmin,
		Name: textx.Sanitize(u.Name),
		SK:   string(s.Kind),
		SX:   s.ExpiresAt.Unix(),
	}
}

// Mint signs a JWT for u's session s and returns it with its expiry.
func (i *Issuer) Mint(u store.User, s store.Session) (string, time.Time, error) {
	c := i.claims(u, s, time.Now())
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, c)
	tok.Header["kid"] = i.kid
	signed, err := tok.SignedString(i.key)
	return signed, c.ExpiresAt.Time, err
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

type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Kid string `json:"kid"`
	X   string `json:"x"`
}

// JWKSHandler publishes the verification keys; a list so a previous key can stay published during a rotation.
func (i *Issuer) JWKSHandler() http.Handler {
	pub := i.key.Public().(ed25519.PublicKey)
	body, err := json.Marshal(map[string][]jwk{"keys": {{Kty: "OKP", Crv: "Ed25519", Alg: "EdDSA", Use: "sig", Kid: i.kid, X: base64.RawURLEncoding.EncodeToString(pub)}}})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
}
