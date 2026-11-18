package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

// Claude Code's public OAuth client; it has no revoke endpoint, so disconnect only drops the tokens.
const (
	ClaudeClientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	ClaudeAuthorizeURL = "https://claude.ai/oauth/authorize"
	ClaudeTokenURL     = "https://console.anthropic.com/v1/oauth/token"
	ClaudeRedirectURL  = "https://console.anthropic.com/oauth/code/callback"
	ClaudeScopes       = "user:profile user:inference"
)

const maxExpiresIn = 365 * 24 * 3600

var ErrInvalidGrant = errors.New("refresh token rejected")

type OAuthTokens struct {
	Access, Refresh string
	ExpiresAt       time.Time
	Email           string
}

func (OAuthTokens) String() string               { return "OAuthTokens{redacted}" }
func (OAuthTokens) GoString() string             { return "OAuthTokens{redacted}" }
func (OAuthTokens) MarshalJSON() ([]byte, error) { return []byte(`"redacted"`), nil }
func (OAuthTokens) LogValue() slog.Value         { return slog.StringValue("redacted") }

type ClaudeOAuth interface {
	AuthorizeURL(state, challenge string) string
	Exchange(ctx context.Context, code, state, verifier string) (OAuthTokens, error)
	Refresh(ctx context.Context, refresh string) (OAuthTokens, error)
}

// A caller-supplied Client is used with redirects disabled, so the code verifier and tokens never follow a redirect.
type HTTPClaudeOAuth struct {
	Client   *http.Client
	TokenURL string
}

func (HTTPClaudeOAuth) AuthorizeURL(state, challenge string) string {
	q := url.Values{
		"code":                  {"true"},
		"response_type":         {"code"},
		"client_id":             {ClaudeClientID},
		"redirect_uri":          {ClaudeRedirectURL},
		"scope":                 {ClaudeScopes},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	return ClaudeAuthorizeURL + "?" + q.Encode()
}

func (c HTTPClaudeOAuth) Exchange(ctx context.Context, code, state, verifier string) (OAuthTokens, error) {
	return c.post(ctx, map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"state":         state,
		"client_id":     ClaudeClientID,
		"redirect_uri":  ClaudeRedirectURL,
		"code_verifier": verifier,
	})
}

func (c HTTPClaudeOAuth) Refresh(ctx context.Context, refresh string) (OAuthTokens, error) {
	t, err := c.post(ctx, map[string]string{"grant_type": "refresh_token", "refresh_token": refresh, "client_id": ClaudeClientID})
	if err == nil && t.Refresh == "" {
		t.Refresh = refresh
	}
	return t, err
}

func (c HTTPClaudeOAuth) post(ctx context.Context, fields map[string]string) (OAuthTokens, error) {
	body, err := json.Marshal(fields)
	if err != nil {
		return OAuthTokens{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, orDefault(c.TokenURL, ClaudeTokenURL), bytes.NewReader(body))
	if err != nil {
		return OAuthTokens{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := noRedirects(c.Client).Do(req)
	if err != nil {
		return OAuthTokens{}, fmt.Errorf("%w: %s: token request failed", ErrUnreachable, Claude)
	}
	defer resp.Body.Close()
	var out struct {
		Error        string `json:"error"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Account      struct {
			EmailAddress string `json:"email_address"`
		} `json:"account"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(&out)
	switch {
	case resp.StatusCode == http.StatusBadRequest && out.Error == "invalid_grant":
		return OAuthTokens{}, ErrInvalidGrant
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 || resp.StatusCode/100 == 3:
		return OAuthTokens{}, fmt.Errorf("%w: %s: HTTP %d", ErrUnreachable, Claude, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return OAuthTokens{}, fmt.Errorf("%w: %s: HTTP %d", ErrRejected, Claude, resp.StatusCode)
	case decodeErr != nil || out.AccessToken == "" || out.ExpiresIn <= 0:
		return OAuthTokens{}, fmt.Errorf("%w: %s: malformed token response", ErrUnreachable, Claude)
	}
	return OAuthTokens{
		Access:    out.AccessToken,
		Refresh:   out.RefreshToken,
		ExpiresAt: time.Now().Add(time.Duration(min(out.ExpiresIn, maxExpiresIn)) * time.Second),
		Email:     out.Account.EmailAddress,
	}, nil
}
