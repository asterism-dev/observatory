// Package edge is the public entry point: it routes requests to the hub, extracted services or the SPA.
package edge

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/asterism-dev/observatory/internal/platform/clientip"
)

const (
	HeaderClientIP = clientip.HeaderClientIP
	HeaderEdge     = clientip.HeaderEdge
)

const minTokenLen = 32

type Config struct {
	// IAMURL serves login (/auth/, /api/auth/) and the token endpoint; its API goes through Services like any service.
	HubURL, IAMURL, Token string
	// ServiceToken authenticates the edge to iam's /internal/iam/ endpoints.
	ServiceToken string
	Services     map[string]string // service name → base URL
}

var hubPrefixes = []string{"/api/", "/v2/", "/otlp/", "/node/", "/grafana/"}

// iamPrefixes reach iam unchanged: login reads the session itself and must see it.
var iamPrefixes = []string{"/auth/", "/api/auth/"}

func New(cfg Config, assets fs.FS) (http.Handler, error) {
	if len(cfg.Token) < minTokenLen {
		return nil, fmt.Errorf("edge token must be at least %d characters", minTokenLen)
	}
	if len(cfg.ServiceToken) < minTokenLen {
		return nil, fmt.Errorf("service token must be at least %d characters", minTokenLen)
	}
	hubURL, err := parseUpstream(cfg.HubURL)
	if err != nil {
		return nil, fmt.Errorf("hub url: %w", err)
	}
	iamURL, err := parseUpstream(cfg.IAMURL)
	if err != nil {
		return nil, fmt.Errorf("iam url: %w", err)
	}
	hub := proxy(hubURL, cfg.Token, false)
	iamLogin := proxy(iamURL, cfg.Token, false)
	id := &identity{
		tokenURL:     iamURL.JoinPath("internal/iam/token").String(),
		serviceToken: cfg.ServiceToken,
		client:       &http.Client{Timeout: 10 * time.Second},
		cache:        newJWTCache(jwtCacheSize),
	}
	hub.ModifyResponse = id.forget
	iamLogin.ModifyResponse = id.forget
	hubAPI := id.hubMiddleware(hub)
	services := make(map[string]http.Handler, len(cfg.Services))
	for name, raw := range cfg.Services {
		u, err := parseUpstream(raw)
		if err != nil {
			return nil, fmt.Errorf("service %s url: %w", name, err)
		}
		p := proxy(u, "", true)
		p.ModifyResponse = id.forget
		services[name] = id.middleware(p)
	}
	app := spa(assets)

	// No ServeMux: it would redirect unclean paths, which /grafana/* needs raw.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Cleaned so dot segments like /api/../internal cannot slip past to the hub.
		if clean := path.Clean(r.URL.Path); clean == "/internal" || strings.HasPrefix(clean, "/internal/") {
			http.NotFound(w, r)
			return
		}
		for _, p := range iamPrefixes {
			if strings.HasPrefix(r.URL.Path, p) {
				iamLogin.ServeHTTP(w, r)
				return
			}
		}
		if rest, ok := strings.CutPrefix(r.URL.Path, "/api/"); ok {
			if name, sub, ok := strings.Cut(rest, "/"); ok && strings.HasPrefix(sub, "v1/") {
				if h, ok := services[name]; ok {
					h.ServeHTTP(w, r)
					return
				}
			}
			hubAPI.ServeHTTP(w, r)
			return
		}
		for _, p := range hubPrefixes {
			if strings.HasPrefix(r.URL.Path, p) {
				hub.ServeHTTP(w, r)
				return
			}
		}
		app.ServeHTTP(w, r)
	}), nil
}

var serviceName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// ParseServices reads comma-separated name=url pairs into Config.Services; empty input yields an empty map.
func ParseServices(env string) (map[string]string, error) {
	services := map[string]string{}
	if strings.TrimSpace(env) == "" {
		return services, nil
	}
	for i, pair := range strings.Split(env, ",") {
		name, raw, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || !serviceName.MatchString(name) {
			return nil, fmt.Errorf("entry %d is not name=url with a lower-case name", i+1)
		}
		if _, err := parseUpstream(raw); err != nil {
			return nil, fmt.Errorf("service %s url: %w", name, err)
		}
		if _, dup := services[name]; dup {
			return nil, fmt.Errorf("duplicate service %q", name)
		}
		services[name] = raw
	}
	return services, nil
}

func parseUpstream(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, errors.New("must be an absolute URL")
	}
	return u, nil
}
