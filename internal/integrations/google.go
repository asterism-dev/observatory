package integrations

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const maxADCSize = 16 << 10

var quotaProjectRE = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)

// ValidQuotaProject accepts the empty string (no quota project) or a GCP project id.
func ValidQuotaProject(s string) bool { return s == "" || quotaProjectRE.MatchString(s) }

type ADC struct{ ClientID, ClientSecret, RefreshToken, QuotaProject string }

func (ADC) String() string       { return "ADC{redacted}" }
func (ADC) GoString() string     { return "ADC{redacted}" }
func (ADC) LogValue() slog.Value { return slog.StringValue("redacted") }

type adcBlob struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
}

func ParseADC(raw string) (ADC, error) {
	if len(raw) > maxADCSize {
		return ADC{}, ErrInvalidToken
	}
	var in struct {
		adcBlob
		Type         string `json:"type"`
		QuotaProject string `json:"quota_project_id"`
	}
	if err := json.Unmarshal([]byte(raw), &in); err != nil || in.Type != "authorized_user" ||
		in.ClientID == "" || in.ClientSecret == "" || in.RefreshToken == "" {
		return ADC{}, ErrInvalidToken
	}
	return ADC{in.ClientID, in.ClientSecret, in.RefreshToken, in.QuotaProject}, nil
}

// Marshal is the canonical blob the hub stores as the refresh token; the quota project is stored separately.
func (a ADC) Marshal() string {
	b, _ := json.Marshal(adcBlob{a.ClientID, a.ClientSecret, a.RefreshToken})
	return string(b)
}

type GoogleOAuth interface {
	Refresh(ctx context.Context, adc string) (OAuthTokens, error)
	Email(ctx context.Context, access string) (string, error)
}

// A caller-supplied Client is used with redirects disabled, so secrets and tokens never follow a redirect.
type HTTPGoogleOAuth struct {
	Client                *http.Client
	TokenURL, UserinfoURL string
}

func (g HTTPGoogleOAuth) Refresh(ctx context.Context, adc string) (OAuthTokens, error) {
	var blob adcBlob
	if err := json.Unmarshal([]byte(adc), &blob); err != nil || blob.ClientID == "" || blob.ClientSecret == "" || blob.RefreshToken == "" {
		return OAuthTokens{}, ErrInvalidToken
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {blob.ClientID},
		"client_secret": {blob.ClientSecret},
		"refresh_token": {blob.RefreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, orDefault(g.TokenURL, "https://oauth2.googleapis.com/token"), strings.NewReader(form.Encode()))
	if err != nil {
		return OAuthTokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := noRedirects(g.Client).Do(req)
	if err != nil {
		return OAuthTokens{}, fmt.Errorf("%w: %s: token request failed", ErrUnreachable, GCloud)
	}
	defer resp.Body.Close()
	var out struct {
		Error       string `json:"error"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(&out)
	switch {
	case resp.StatusCode == http.StatusBadRequest && out.Error == "invalid_grant":
		return OAuthTokens{}, ErrInvalidGrant
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 || resp.StatusCode/100 == 3:
		return OAuthTokens{}, fmt.Errorf("%w: %s: HTTP %d", ErrUnreachable, GCloud, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return OAuthTokens{}, fmt.Errorf("%w: %s: HTTP %d", ErrRejected, GCloud, resp.StatusCode)
	case decodeErr != nil || out.AccessToken == "" || out.ExpiresIn <= 0:
		return OAuthTokens{}, fmt.Errorf("%w: %s: malformed token response", ErrUnreachable, GCloud)
	}
	return OAuthTokens{
		Access:    out.AccessToken,
		Refresh:   adc,
		ExpiresAt: time.Now().Add(time.Duration(min(out.ExpiresIn, maxExpiresIn)) * time.Second),
	}, nil
}

func (g HTTPGoogleOAuth) Email(ctx context.Context, access string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, orDefault(g.UserinfoURL, "https://openidconnect.googleapis.com/v1/userinfo"), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	var body struct{ Email string }
	if _, err := do(g.Client, req, GCloud, &body); err != nil {
		return "", err
	}
	return truncate(body.Email), nil
}
