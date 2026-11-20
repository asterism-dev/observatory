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

// BuildCredentials resolves a per-build secret to its running build, else store.ErrNotFound.
type BuildCredentials interface {
	BuildBySecret(ctx context.Context, secret string) (store.ImageBuild, error)
}

type Registry struct {
	Upstream, PublicURL *url.URL
	// OtherURLs are the further hub URLs clients reach /v2/ at; a request to one of their hosts gets its token realm there.
	OtherURLs    []*url.URL
	Issuer       registry.Issuer
	Builds       BuildCredentials
	Manifests    RegistryManifests
	Tags         *TagSync
	EventsSecret string
	GC           *registry.GC
}

// RegistryEndpoints serves the token endpoint and the registry's push events, and proxies the rest of /v2/ to the registry.
func RegistryEndpoints(mux *http.ServeMux, s *store.Store, z *authz.Service, reg Registry) {
	t := &registryTokens{store: s, z: z, reg: reg}
	mux.HandleFunc("GET /v2/token", t.token)
	ev := &registryEvents{store: s, manifests: reg.Manifests, tags: reg.Tags, secret: reg.EventsSecret}
	mux.HandleFunc("POST /internal/registry/events", ev.serve)
	mux.Handle("/v2/", registryProxy(reg.Upstream, reg.PublicURL, reg.OtherURLs))
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
	for _, a := range access {
		if slices.Contains(a.Actions, "push") && !t.reg.GC.PushAllowed() {
			w.Header().Set("Retry-After", "300")
			writeError(w, http.StatusServiceUnavailable, "the registry is being cleaned up; push again in a few minutes")
			return
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
	case p.build.ID != uuid.Nil && img.ID == p.build.ImageID:
		pull, push = true, true
	case p.build.ID != uuid.Nil:
		if pull, err = t.buildMayPull(ctx, p.build, img); err != nil {
			return a, err
		}
	case img.Scope != store.EnvScopeUser || *img.OwnerID == p.user:
		sub, obj := authz.UserSubject(p.user), imageObject(img.Scope, img.OwnerID)
		pull, push = t.z.Allowed(sub, img.OrgID, obj, "image:use"), t.z.Allowed(sub, img.OrgID, obj, "image:build")
	}
	if push && slices.Contains(requested, "push") {
		over, err := overImageStorageLimit(ctx, t.store, img.OrgID)
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

func overImageStorageLimit(ctx context.Context, s *store.Store, org uuid.UUID) (bool, error) {
	ls, err := s.Limits(ctx, org)
	if err != nil {
		return false, err
	}
	limit := ls[store.ScopeOrg].ImageStorageGB
	if limit == nil {
		return false, nil
	}
	used, err := s.OrgImageUsage(ctx, org)
	return used >= int64(*limit)<<30, err
}

// buildMayPull lets a build pull base images at its image's level or broader that the build's starter may use.
func (t *registryTokens) buildMayPull(ctx context.Context, b store.ImageBuild, img store.Image) (bool, error) {
	built, err := t.store.ImageByID(ctx, b.ImageID)
	if err != nil {
		return false, err
	}
	if img.OrgID != built.OrgID || !refAllowed(store.RecipeRef{Scope: img.Scope, OwnerID: img.OwnerID}, built.Scope, built.OwnerID) {
		return false, nil
	}
	return t.z.Allowed(authz.UserSubject(b.StartedBy), img.OrgID, imageObject(img.Scope, img.OwnerID), "image:use"), nil
}

var realmPattern = regexp.MustCompile(`realm="[^"]*"`)

type realmKey struct{}

// registryProxy points the token realm at the configured URL whose host the request came in on, never at the Host header itself.
func registryProxy(upstream, public *url.URL, others []*url.URL) http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) { pr.SetURL(upstream) },
		ModifyResponse: func(resp *http.Response) error {
			if v := resp.Header.Get("WWW-Authenticate"); v != "" {
				realm := resp.Request.Context().Value(realmKey{}).(string)
				resp.Header.Set("WWW-Authenticate", realmPattern.ReplaceAllLiteralString(v, `realm="`+realm+`"`))
			}
			return nil
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := public
		for _, u := range others {
			if strings.EqualFold(u.Host, r.Host) {
				base = u
				break
			}
		}
		r = r.WithContext(context.WithValue(r.Context(), realmKey{}, base.JoinPath("/v2/token").String()))
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
