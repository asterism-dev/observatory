package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

type Account struct {
	Name, Scopes string
	ExpiresAt    *time.Time
}

type Verifier interface {
	Verify(ctx context.Context, kind Kind, source Source, token string) (Account, error)
}

var (
	ErrRejected    = errors.New("provider rejected the token")
	ErrUnreachable = errors.New("provider unreachable")
)

const maxResponse = 1 << 20

// A caller-supplied Client is used with redirects disabled, so a token never follows a redirect.
type HTTPVerifier struct {
	Client                             *http.Client
	GitHubAPI, LinearAPI, AnthropicAPI string
}

func (v HTTPVerifier) Verify(ctx context.Context, kind Kind, source Source, token string) (Account, error) {
	if !slices.Contains(Kinds[kind].Sources, source) {
		return Account{}, fmt.Errorf("%w: %s: unsupported source", ErrRejected, kind)
	}
	switch kind {
	case GitHub:
		return v.github(ctx, token)
	case Linear:
		return v.linear(ctx, token)
	case GCloud:
		return Account{}, fmt.Errorf("%w: %s: verified through Google OAuth", ErrRejected, kind)
	default:
		return v.claude(ctx, source, token)
	}
}

func (v HTTPVerifier) github(ctx context.Context, token string) (Account, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, orDefault(v.GitHubAPI, "https://api.github.com")+"/user", nil)
	if err != nil {
		return Account{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	var body struct{ Login string }
	resp, err := do(v.Client, req, GitHub, &body)
	if err != nil {
		return Account{}, err
	}
	return Account{
		Name:      truncate(body.Login),
		Scopes:    truncate(resp.Header.Get("X-OAuth-Scopes")),
		ExpiresAt: parseGitHubExpiry(resp.Header.Get("github-authentication-token-expiration")),
	}, nil
}

func parseGitHubExpiry(s string) *time.Time {
	for _, layout := range []string{"2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 -0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			t = t.UTC()
			return &t
		}
	}
	return nil
}

func (v HTTPVerifier) linear(ctx context.Context, token string) (Account, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, orDefault(v.LinearAPI, "https://api.linear.app")+"/graphql",
		strings.NewReader(`{"query":"{ viewer { email } }"}`))
	if err != nil {
		return Account{}, err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	var body struct {
		Data struct{ Viewer *struct{ Email string } }
	}
	if _, err := do(v.Client, req, Linear, &body); err != nil {
		return Account{}, err
	}
	// Linear may answer 200 with only GraphQL errors.
	if body.Data.Viewer == nil {
		return Account{}, fmt.Errorf("%w: %s: no viewer", ErrRejected, Linear)
	}
	return Account{Name: truncate(body.Data.Viewer.Email)}, nil
}

func (v HTTPVerifier) claude(ctx context.Context, source Source, token string) (Account, error) {
	if source == SourceSetupToken && !strings.HasPrefix(token, "sk-ant-oat") {
		return Account{}, fmt.Errorf("%w: %s: not a setup token", ErrRejected, Claude)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, orDefault(v.AnthropicAPI, "https://api.anthropic.com")+"/v1/models", nil)
	if err != nil {
		return Account{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	if _, err := do(v.Client, req, Claude, nil); err != nil {
		return Account{}, err
	}
	return Account{}, nil
}

// Errors carry only kind and a fixed reason; transport errors and provider bodies are dropped so no token can leak.
func do(c *http.Client, req *http.Request, kind Kind, out any) (*http.Response, error) {
	resp, err := noRedirects(c).Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: request failed", ErrUnreachable, kind)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return nil, fmt.Errorf("%w: %s: HTTP %d", ErrUnreachable, kind, resp.StatusCode)
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("%w: %s: HTTP %d", ErrRejected, kind, resp.StatusCode)
	case resp.StatusCode >= 300:
		return nil, fmt.Errorf("%w: %s: HTTP %d", ErrUnreachable, kind, resp.StatusCode)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse))
	} else {
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(out); err != nil {
			return nil, fmt.Errorf("%w: %s: malformed response", ErrUnreachable, kind)
		}
	}
	return resp, nil
}

// Go forwards Authorization to subdomains and re-sends bodies on 307/308; a 3xx must end as ErrUnreachable instead.
func noRedirects(c *http.Client) *http.Client {
	cp := http.Client{Timeout: 15 * time.Second}
	if c != nil {
		cp = *c
	}
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cp
}

const maxAccountField = 256

func truncate(s string) string {
	if len(s) <= maxAccountField {
		return s
	}
	return strings.ToValidUTF8(s[:maxAccountField], "")
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return strings.TrimRight(s, "/")
}

// SkipVerifier accepts every token; dev mode only.
type SkipVerifier struct{}

func (SkipVerifier) Verify(context.Context, Kind, Source, string) (Account, error) {
	return Account{Name: "unverified"}, nil
}
