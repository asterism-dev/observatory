package identity

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/asterism-dev/observatory/internal/platform/textx"
)

const tokenTTL = 5 * time.Minute

// Signer mints the identity JWTs a Verifier accepts and publishes their key set.
type Signer struct {
	key    ed25519.PrivateKey
	kid    string
	issuer string
}

// NewSigner signs with key as issuer, compared without a trailing slash as the Verifier does.
func NewSigner(key ed25519.PrivateKey, issuer string) (*Signer, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("identity: signing key has %d bytes, want %d", len(key), ed25519.PrivateKeySize)
	}
	return &Signer{key: key, kid: thumbprint(key.Public().(ed25519.PublicKey)), issuer: strings.TrimRight(issuer, "/")}, nil
}

// Verifier checks tokens s signed without fetching a key set, for the service that holds the key.
func (s *Signer) Verifier() *Verifier {
	v := &Verifier{issuer: s.issuer, now: time.Now, local: true}
	v.set.Store(&keySet{keys: map[string]ed25519.PublicKey{s.kid: s.key.Public().(ed25519.PublicKey)}})
	return v
}

// thumbprint is the RFC 7638 JWK thumbprint, so the kid is stable for a key and changes with it.
func thumbprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256([]byte(`{"crv":"Ed25519","kty":"OKP","x":"` + base64.RawURLEncoding.EncodeToString(pub) + `"}`))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Mint signs a JWT for p and returns it with its expiry.
func (s *Signer) Mint(p Principal) (string, time.Time, error) {
	now := time.Now().Truncate(time.Second)
	exp := now.Add(tokenTTL)
	// A token must not outlive its session; truncation keeps exp at or before the session's expiry.
	if end := p.SessionExpires.Truncate(time.Second); !p.SessionExpires.IsZero() && end.Before(exp) {
		exp = end
	}
	c := claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.issuer,
			Audience:  jwt.ClaimStrings{audience},
			Subject:   p.User.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
		Session:       p.Session.String(),
		PlatformAdmin: p.PlatformAdmin,
		Name:          textx.Sanitize(p.Name),
		SessionKind:   p.SessionKind,
	}
	if !p.SessionExpires.IsZero() {
		c.SessionExp = p.SessionExpires.Unix()
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, c)
	tok.Header["kid"] = s.kid
	signed, err := tok.SignedString(s.key)
	return signed, exp, err
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
func (s *Signer) JWKSHandler() http.Handler {
	pub := s.key.Public().(ed25519.PublicKey)
	body, err := json.Marshal(map[string][]jwk{"keys": {{Kty: "OKP", Crv: "Ed25519", Alg: "EdDSA", Use: "sig", Kid: s.kid, X: base64.RawURLEncoding.EncodeToString(pub)}}})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
}
