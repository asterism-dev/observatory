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
	HubURL, Token string
	// ServiceToken authenticates the edge to the hub's /internal/iam/ endpoints.
	ServiceToken string
	Services     map[string]string // service name → base URL
}

var hubPrefixes = []string{"/api/", "/auth/", "/v2/", "/otlp/", "/node/", "/grafana/"}

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
	hub := proxy(hubURL, cfg.Token, false)
	id := &identity{
		tokenURL:     hubURL.JoinPath("internal/iam/token").String(),
		serviceToken: cfg.ServiceToken,
		client:       &http.Client{Timeout: 10 * time.Second},
		cache:        newJWTCache(jwtCacheSize),
	}
	services := make(map[string]http.Handler, len(cfg.Services))
	for name, raw := range cfg.Services {
		u, err := parseUpstream(raw)
		if err != nil {
			return nil, fmt.Errorf("service %s url: %w", name, err)
		}
		services[name] = id.middleware(proxy(u, "", true))
	}
	app := spa(assets)

	// No ServeMux: it would redirect unclean paths, which /grafana/* needs raw.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Cleaned so dot segments like /api/../internal cannot slip past to the hub.
		if clean := path.Clean(r.URL.Path); clean == "/internal" || strings.HasPrefix(clean, "/internal/") {
			http.NotFound(w, r)
			return
		}
		if rest, ok := strings.CutPrefix(r.URL.Path, "/api/"); ok {
			if name, sub, ok := strings.Cut(rest, "/"); ok && strings.HasPrefix(sub, "v1/") {
				if h, ok := services[name]; ok {
					h.ServeHTTP(w, r)
					return
				}
			}
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
