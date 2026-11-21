package hubserver

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
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/servicetoken"
	"github.com/asterism-dev/observatory/internal/store"
)

const (
	signingKeySecret = "iam-signing-key"
	jwtAudience      = "observatory"
	jwtTTL           = 5 * time.Minute
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
	SID string `json:"sid"`
	PA  bool   `json:"pa"`
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
		now := time.Now().Truncate(time.Second)
		claims := userClaims{
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer:    i.url,
				Audience:  jwt.ClaimStrings{jwtAudience},
				Subject:   user.ID.String(),
				IssuedAt:  jwt.NewNumericDate(now),
				NotBefore: jwt.NewNumericDate(now),
				ExpiresAt: jwt.NewNumericDate(now.Add(jwtTTL)),
			},
			SID: sess.ID.String(),
			PA:  user.PlatformAdmin,
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
		tok.Header["kid"] = i.kid
		signed, err := tok.SignedString(i.key)
		if err != nil {
			slog.Error("iam token: sign", "caller", caller, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"token": signed, "expires_at": now.Add(jwtTTL)})
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

// PolicyServer streams the authorization snapshot; Publish never blocks, and a slow watcher skips to the latest snapshot.
type PolicyServer struct {
	mu   sync.Mutex
	rev  uint64
	last *iamv1.Policy
	subs map[chan struct{}]struct{}
}

func NewPolicyServer() *PolicyServer {
	// Seeding with the clock keeps revisions increasing across hub restarts, so watchers never see one go backwards.
	return &PolicyServer{rev: uint64(time.Now().UnixMicro()), subs: map[chan struct{}]struct{}{}}
}

// Publish is fed from authz.Service.OnReload.
func (p *PolicyServer) Publish(pol authz.Policy) {
	msg := policyMessage(pol)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rev++
	msg.Revision = p.rev
	p.last = msg
	for ch := range p.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (p *PolicyServer) latest() *iamv1.Policy {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

func (p *PolicyServer) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	ch <- struct{}{}
	p.mu.Lock()
	p.subs[ch] = struct{}{}
	p.mu.Unlock()
	return ch
}

func (p *PolicyServer) unsubscribe(ch chan struct{}) {
	p.mu.Lock()
	delete(p.subs, ch)
	p.mu.Unlock()
}

func (p *PolicyServer) WatchPolicy(ctx context.Context, _ *connect.Request[iamv1.WatchPolicyRequest], stream *connect.ServerStream[iamv1.Policy]) error {
	caller, _ := servicetoken.Caller(ctx)
	slog.Info("policy watch started", "caller", caller)
	defer slog.Info("policy watch ended", "caller", caller)
	ch := p.subscribe()
	defer p.unsubscribe(ch)
	var sent uint64
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ch:
		}
		msg := p.latest()
		if msg == nil || msg.Revision == sent {
			continue
		}
		if err := stream.Send(msg); err != nil {
			return err
		}
		sent = msg.Revision
	}
}

func policyMessage(p authz.Policy) *iamv1.Policy {
	m := &iamv1.Policy{}
	for _, r := range p.Memberships {
		m.Memberships = append(m.Memberships, &iamv1.MemberRole{OrgId: r.OrgID.String(), UserId: r.UserID.String(), Role: r.Role})
	}
	for _, t := range p.TeamMembers {
		m.TeamMembers = append(m.TeamMembers, &iamv1.TeamMember{OrgId: t.OrgID.String(), TeamId: t.TeamID.String(), UserId: t.UserID.String(), Admin: t.Admin})
	}
	for _, r := range p.Roles {
		role := &iamv1.CustomRole{Id: r.ID.String(), OrgId: r.OrgID.String(), Name: r.Name, Description: r.Description, Parents: r.Parents}
		for _, rule := range r.Rules {
			role.Rules = append(role.Rules, &iamv1.Rule{Effect: string(rule.Effect), Object: rule.Object, Action: rule.Action})
		}
		m.Roles = append(m.Roles, role)
	}
	for _, b := range p.Bindings {
		m.Bindings = append(m.Bindings, &iamv1.Binding{OrgId: b.OrgID.String(), Subject: b.Subject, RoleId: b.RoleID.String()})
	}
	for _, t := range p.Teams {
		m.Teams = append(m.Teams, &iamv1.TeamRef{OrgId: t.OrgID.String(), Id: t.ID.String()})
	}
	return m
}

// internalIAM serves /internal/iam/*; the caller wraps it in servicetoken.Middleware.
func internalIAM(iss *Issuer, ps *PolicyServer) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/internal/iam/token", iss.TokenHandler())
	mux.Handle("/internal/iam/jwks", iss.JWKSHandler())
	path, h := iamv1connect.NewPolicyServiceHandler(ps)
	stream := http.StripPrefix("/internal/iam", h)
	mux.Handle("/internal/iam"+path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The hub server's read/write timeouts would cut the long-lived stream.
		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(time.Time{})
		_ = rc.SetWriteDeadline(time.Time{})
		stream.ServeHTTP(w, r)
	}))
	return mux
}
