package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/registry"
	"github.com/asterism-dev/observatory/internal/store"
)

const (
	registryUser          = "observatory"
	registryCredentialTTL = 12 * time.Hour
)

// BuildCredentials resolves a per-build secret to its queued or running build, else store.ErrNotFound.
type BuildCredentials interface {
	BuildBySecret(ctx context.Context, secret string) (store.ImageBuild, error)
}

type Registry struct {
	Upstream, PublicURL *url.URL
	Issuer              registry.Issuer
	Builds              BuildCredentials
}

// RegistryEndpoints serves the token endpoint and proxies the rest of /v2/ to the registry.
func RegistryEndpoints(mux *http.ServeMux, s *store.Store, z *authz.Service, reg Registry) {
	t := &registryTokens{store: s, z: z, reg: reg}
	mux.HandleFunc("GET /v2/token", t.token)
	mux.Handle("/v2/", registryProxy(reg.Upstream, reg.PublicURL.JoinPath("/v2/token").String()))
}

type registryTokens struct {
	store *store.Store
	z     *authz.Service
	reg   Registry
}

// registryPrincipal is a user, or a builder when build.ID is set.
type registryPrincipal struct {
	user  uuid.UUID
	build store.ImageBuild
}

func (p registryPrincipal) subject() string {
	if p.build.ID != uuid.Nil {
		return "build:" + p.build.ID.String()
	}
	return authz.UserSubject(p.user)
}

func (t *registryTokens) token(w http.ResponseWriter, r *http.Request) {
	p, err := t.principal(r)
	if errors.Is(err, store.ErrNotFound) {
		w.Header().Set("WWW-Authenticate", `Basic realm="observatory"`)
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	} else if err != nil {
		serverError(w, "registry principal", err)
		return
	}
	var access []registry.Access
	for _, raw := range r.URL.Query()["scope"] {
		for _, scope := range strings.Fields(raw) {
			a, err := t.grant(r.Context(), p, scope)
			if err != nil {
				serverError(w, "registry grant", err)
				return
			}
			if len(a.Actions) > 0 {
				access = append(access, a)
			}
		}
	}
	tok, err := t.reg.Issuer.Token(p.subject(), access, registry.TokenTTL)
	if err != nil {
		serverError(w, "registry token", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "access_token": tok,
		"expires_in": int(registry.TokenTTL.Seconds()), "issued_at": time.Now().UTC().Format(time.RFC3339)})
}

func (t *registryTokens) principal(r *http.Request) (registryPrincipal, error) {
	user, secret, ok := r.BasicAuth()
	if !ok || user != registryUser || secret == "" {
		return registryPrincipal{}, store.ErrNotFound
	}
	id, err := t.store.RegistryCredentialUser(r.Context(), secret)
	if !errors.Is(err, store.ErrNotFound) || t.reg.Builds == nil {
		return registryPrincipal{user: id}, err
	}
	b, err := t.reg.Builds.BuildBySecret(r.Context(), secret)
	return registryPrincipal{build: b}, err
}

// grant returns the requested actions on scope the principal holds; anything unknown yields none.
func (t *registryTokens) grant(ctx context.Context, p registryPrincipal, scope string) (registry.Access, error) {
	typ, rest, _ := strings.Cut(scope, ":")
	i := strings.LastIndex(rest, ":")
	if typ != "repository" || i < 0 {
		return registry.Access{}, nil
	}
	a, requested := registry.Access{Type: typ, Name: rest[:i]}, strings.Split(rest[i+1:], ",")
	raw, ok := strings.CutPrefix(a.Name, "i/")
	id, err := uuid.Parse(raw)
	if !ok || err != nil || id.String() != raw {
		return a, nil
	}
	img, err := t.store.ImageByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return a, nil
	} else if err != nil {
		return a, err
	}
	var pull, push bool
	switch {
	case p.build.ID != uuid.Nil:
		pull, push = img.ID == p.build.ImageID, img.ID == p.build.ImageID
	case img.Scope != store.EnvScopeUser || *img.OwnerID == p.user:
		sub, obj := authz.UserSubject(p.user), imageObject(img.Scope, img.OwnerID)
		pull, push = t.z.Allowed(sub, img.OrgID, obj, "image:use"), t.z.Allowed(sub, img.OrgID, obj, "image:build")
	}
	if push && slices.Contains(requested, "push") {
		over, err := t.overStorageLimit(ctx, img.OrgID)
		if err != nil {
			return a, err
		}
		push = !over
	}
	if pull && slices.Contains(requested, "pull") {
		a.Actions = append(a.Actions, "pull")
	}
	if push && slices.Contains(requested, "push") {
		a.Actions = append(a.Actions, "push")
	}
	return a, nil
}

func (t *registryTokens) overStorageLimit(ctx context.Context, org uuid.UUID) (bool, error) {
	ls, err := t.store.Limits(ctx, org)
	if err != nil {
		return false, err
	}
	limit := ls[store.ScopeOrg].ImageStorageGB
	if limit == nil {
		return false, nil
	}
	used, err := t.store.OrgImageUsage(ctx, org)
	return used >= int64(*limit)<<30, err
}

var realmPattern = regexp.MustCompile(`realm="[^"]*"`)

func registryProxy(upstream *url.URL, realm string) http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) { pr.SetURL(upstream) },
		ModifyResponse: func(resp *http.Response) error {
			if v := resp.Header.Get("WWW-Authenticate"); v != "" {
				resp.Header.Set("WWW-Authenticate", realmPattern.ReplaceAllLiteralString(v, `realm="`+realm+`"`))
			}
			return nil
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Blob transfers outlast the hub's server-wide read and write timeouts.
		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(time.Time{})
		_ = rc.SetWriteDeadline(time.Time{})
		rp.ServeHTTP(w, r)
	})
}

func (h *api) createRegistryCredential(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	if p.Session.Kind != store.SessionCLI {
		writeError(w, http.StatusForbidden, "registry credentials are issued to CLI sessions only")
		return
	}
	secret, expires, err := h.store.CreateRegistryCredential(r.Context(), p.Session.ID, registryCredentialTTL)
	if err != nil {
		serverError(w, "registry credential", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"username": registryUser, "secret": secret, "expiresAt": expires})
}
