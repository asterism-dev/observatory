// Package servicetoken authenticates service-to-service calls with static per-service bearer tokens until iam issues service accounts.
package servicetoken

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
)

const minLen = 32

// Parse reads comma-separated name=token pairs into a name → token map; empty input yields an empty map.
func Parse(env string) (map[string]string, error) {
	tokens := map[string]string{}
	seen := map[string]bool{}
	if strings.TrimSpace(env) == "" {
		return tokens, nil
	}
	for i, pair := range strings.Split(env, ",") {
		name, token, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("entry %d is not name=token", i+1)
		}
		if len(token) < minLen {
			return nil, fmt.Errorf("token for %q must be at least %d characters", name, minLen)
		}
		if _, dup := tokens[name]; dup {
			return nil, fmt.Errorf("duplicate service %q", name)
		}
		// A shared token would make the caller name, and thus the audit trail, ambiguous.
		if seen[token] {
			return nil, fmt.Errorf("token for %q is already used by another service", name)
		}
		seen[token] = true
		tokens[name] = token
	}
	return tokens, nil
}

type callerKey struct{}

// Caller returns the name of the service Middleware authenticated.
func Caller(ctx context.Context) (string, bool) {
	name, ok := ctx.Value(callerKey{}).(string)
	return name, ok
}

// Middleware refuses requests without a known bearer token and records the calling service's name in the context.
func Middleware(tokens map[string]string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			name, ok := match(tokens, r.Header.Get("Authorization"))
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, name)))
		})
	}
}

func match(tokens map[string]string, header string) (string, bool) {
	scheme, got, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || got == "" {
		return "", false
	}
	var caller string
	// Compare against every token so the response time does not reveal which one matched.
	for name, want := range tokens {
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
			caller = name
		}
	}
	return caller, caller != ""
}
