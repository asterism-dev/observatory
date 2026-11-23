package integrations

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/asterism-dev/observatory/internal/egress"
	"github.com/google/uuid"
)

const (
	dummyGitHub  = "ghp_dummy0000000000000000"
	dummyLinear  = "lin_api_dummy000000000000"
	dummyClaude  = "sk-ant-oat01-dummy000000000000"
	dummyRefresh = "sk-ant-ort01-dummy000000000000"
)

func TestNormalizeToken(t *testing.T) {
	got, err := NormalizeToken(" " + dummyGitHub + "\n")
	if err != nil || got != dummyGitHub {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, bad := range []string{"ghp_a b0000000000", "ghp\n00000000000", strings.Repeat("a", 11), strings.Repeat("a", 4097), ""} {
		if _, err := NormalizeToken(bad); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, err := NormalizeToken(strings.Repeat("a", 4096)); err != nil {
		t.Errorf("4096 bytes: %v", err)
	}
}

func TestKindsTable(t *testing.T) {
	want := map[Kind][]string{
		GitHub: {"api.github.com", "github.com", "uploads.github.com"},
		Claude: {"api.anthropic.com"},
		Linear: {"api.linear.app", "mcp.linear.app"},
		GCloud: {"*.googleapis.com", "*.pkg.dev"},
	}
	if len(Kinds) != len(want) {
		t.Fatalf("kinds: %v", Kinds)
	}
	for k, hosts := range want {
		spec, ok := Kinds[k]
		if !ok || !reflect.DeepEqual(spec.Hosts, hosts) || !reflect.DeepEqual(spec.Headers, []string{"Authorization"}) {
			t.Errorf("%s: %+v", k, spec)
		}
		if norm, err := egress.NormalizeHostSet(spec.Hosts); err != nil || !reflect.DeepEqual(norm, spec.Hosts) {
			t.Errorf("%s: normalized %v, %v", k, norm, err)
		}
		if !strings.HasPrefix(spec.TokenPage, "https://") {
			t.Errorf("%s: token page %q", k, spec.TokenPage)
		}
	}
	if !reflect.DeepEqual(Kinds[Claude].Sources, []Source{SourceSetupToken, SourceOAuth}) {
		t.Errorf("claude sources: %v", Kinds[Claude].Sources)
	}
}

func noToken(t *testing.T, err error, tokens ...string) {
	t.Helper()
	for _, tok := range tokens {
		if err != nil && strings.Contains(err.Error(), tok) {
			t.Errorf("error contains token: %v", err)
		}
	}
}

func TestVerifyGitHub(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/user" || r.Header.Get("Authorization") != "Bearer "+dummyGitHub {
			t.Errorf("request: %s %s %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("X-OAuth-Scopes", "repo")
		w.Header().Set("github-authentication-token-expiration", "2026-12-31 00:00:00 UTC")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"login":"octocat","echo":%q}`, dummyGitHub)
	}))
	defer srv.Close()
	v := HTTPVerifier{GitHubAPI: srv.URL}
	ctx := context.Background()

	acc, err := v.Verify(ctx, GitHub, SourcePAT, dummyGitHub)
	want := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	if err != nil || acc.Name != "octocat" || acc.Scopes != "repo" || acc.ExpiresAt == nil || !acc.ExpiresAt.Equal(want) {
		t.Fatalf("got %+v, %v", acc, err)
	}

	status = http.StatusUnauthorized
	_, err = v.Verify(ctx, GitHub, SourcePAT, dummyGitHub)
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("401: %v", err)
	}
	noToken(t, err, dummyGitHub)

	status = http.StatusBadGateway
	_, err = v.Verify(ctx, GitHub, SourcePAT, dummyGitHub)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("502: %v", err)
	}
	noToken(t, err, dummyGitHub)

	srv.Close()
	_, err = v.Verify(ctx, GitHub, SourcePAT, dummyGitHub)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("closed: %v", err)
	}
	noToken(t, err, dummyGitHub)
}

func TestGitHubExpiryFormats(t *testing.T) {
	want := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	if got := parseGitHubExpiry("2026-12-31 01:00:00 +0100"); got == nil || !got.Equal(want) {
		t.Errorf("offset form: %v", got)
	}
	if got := parseGitHubExpiry("soon"); got != nil {
		t.Errorf("unparsable: %v", got)
	}
}

func TestVerifyLinear(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var q struct{ Query string }
		_ = json.Unmarshal(body, &q)
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" || r.Header.Get("Authorization") != dummyLinear || q.Query != "{ viewer { email } }" {
			t.Errorf("request: %s %s %q %s", r.Method, r.URL.Path, r.Header.Get("Authorization"), body)
		}
		fmt.Fprint(w, `{"data":{"viewer":{"email":"bot@example.com"}}}`)
	}))
	defer srv.Close()
	acc, err := HTTPVerifier{LinearAPI: srv.URL}.Verify(context.Background(), Linear, SourceAPIKey, dummyLinear)
	if err != nil || acc.Name != "bot@example.com" {
		t.Fatalf("got %+v, %v", acc, err)
	}
}

func TestVerifyClaude(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer "+dummyClaude ||
			r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
			t.Errorf("request: %s %v", r.URL.Path, r.Header)
		}
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer srv.Close()
	v := HTTPVerifier{AnthropicAPI: srv.URL}

	_, err := v.Verify(context.Background(), Claude, SourceSetupToken, "sk-ant-api03-dummy000000000")
	if !errors.Is(err, ErrRejected) || calls != 0 {
		t.Fatalf("no prefix: %v, calls %d", err, calls)
	}
	acc, err := v.Verify(context.Background(), Claude, SourceSetupToken, dummyClaude)
	if err != nil || acc.Name != "" || calls != 1 {
		t.Fatalf("got %+v, %v, calls %d", acc, err, calls)
	}
}

func TestVerifyUnknownSource(t *testing.T) {
	if _, err := (HTTPVerifier{}).Verify(context.Background(), GitHub, SourceAPIKey, dummyGitHub); !errors.Is(err, ErrRejected) {
		t.Fatalf("got %v", err)
	}
}

func TestClaudeExchangeAndRefresh(t *testing.T) {
	var status int
	var body string
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = nil
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request: %s %v", r.Method, r.Header)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	c := HTTPClaudeOAuth{TokenURL: srv.URL}
	ctx := context.Background()

	status = http.StatusOK
	body = fmt.Sprintf(`{"access_token":%q,"refresh_token":%q,"expires_in":28800,"account":{"email_address":"me@example.com"}}`, dummyClaude, dummyRefresh)
	tok, err := c.Exchange(ctx, "the-code", "the-state", "the-verifier")
	if err != nil || tok.Access != dummyClaude || tok.Refresh != dummyRefresh || tok.Email != "me@example.com" {
		t.Fatalf("exchange: %v", err)
	}
	body = fmt.Sprintf(`{"access_token":%q,"refresh_token":%q,"expires_in":28800,"account":{"email_address":%q}}`, dummyClaude, dummyRefresh, strings.Repeat("é", 200))
	if long, err := c.Exchange(ctx, "the-code", "the-state", "the-verifier"); err != nil || len(long.Email) > 256 || !utf8.ValidString(long.Email) {
		t.Fatalf("long email: %d %v", len(long.Email), err)
	}
	if d := time.Until(tok.ExpiresAt) - 8*time.Hour; d > time.Minute || d < -time.Minute {
		t.Errorf("expires at %v", tok.ExpiresAt)
	}
	wantEx := map[string]string{
		"grant_type": "authorization_code", "code": "the-code", "state": "the-state",
		"client_id": ClaudeClientID, "redirect_uri": ClaudeRedirectURL, "code_verifier": "the-verifier",
	}
	if !reflect.DeepEqual(got, wantEx) {
		t.Errorf("exchange body: %v", got)
	}
	for _, s := range []string{fmt.Sprint(tok), fmt.Sprintf("%+v", tok), fmt.Sprintf("%#v", tok)} {
		if strings.Contains(s, dummyClaude) || strings.Contains(s, dummyRefresh) {
			t.Errorf("printed tokens: %s", s)
		}
	}

	tok, err = c.Refresh(ctx, dummyRefresh)
	if err != nil || tok.Access != dummyClaude {
		t.Fatalf("refresh: %v", err)
	}
	if !reflect.DeepEqual(got, map[string]string{"grant_type": "refresh_token", "refresh_token": dummyRefresh, "client_id": ClaudeClientID}) {
		t.Errorf("refresh body: %v", got)
	}

	status, body = http.StatusBadRequest, `{"error":"invalid_grant","error_description":"`+dummyRefresh+`"}`
	_, err = c.Refresh(ctx, dummyRefresh)
	if !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("400: %v", err)
	}
	noToken(t, err, dummyRefresh)

	status, body = http.StatusServiceUnavailable, ""
	_, err = c.Refresh(ctx, dummyRefresh)
	if err == nil || errors.Is(err, ErrInvalidGrant) || !errors.Is(err, ErrUnreachable) {
		t.Fatalf("503: %v", err)
	}
	noToken(t, err, dummyRefresh)
}

func TestAuthorizeURL(t *testing.T) {
	user, org := uuid.New(), uuid.New()
	s := NewSignIns[string](10*time.Minute, time.Now)
	state, challenge, err := s.Start(user, org, "team")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(HTTPClaudeOAuth{}.AuthorizeURL(state, challenge))
	if err != nil || u.Scheme+"://"+u.Host+u.Path != ClaudeAuthorizeURL {
		t.Fatalf("url %v, %v", u, err)
	}
	q := u.Query()
	want := map[string]string{
		"code": "true", "response_type": "code", "client_id": ClaudeClientID, "redirect_uri": ClaudeRedirectURL,
		"scope": ClaudeScopes, "code_challenge": challenge, "code_challenge_method": "S256", "state": state,
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	verifier, v, ok := s.Take(state, user, org)
	sum := sha256.Sum256([]byte(verifier))
	if !ok || v != "team" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
		t.Fatalf("take: %q %q %v", verifier, v, ok)
	}
	if len(verifier) != 43 || len(state) != 43 {
		t.Errorf("lengths: %d %d", len(verifier), len(state))
	}
}

func TestSignIns(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := NewSignIns[int](10*time.Minute, func() time.Time { return now })
	user, org := uuid.New(), uuid.New()
	state, _, err := s.Start(user, org, 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Take(state, uuid.New(), org); ok {
		t.Fatal("other user took it")
	}
	if _, _, ok := s.Take(state, user, uuid.New()); ok {
		t.Fatal("other org took it")
	}
	if _, v, ok := s.Take(state, user, org); !ok || v != 7 {
		t.Fatalf("owner: %v %v", v, ok)
	}
	if _, _, ok := s.Take(state, user, org); ok {
		t.Fatal("second take")
	}

	state, _, _ = s.Start(user, org, 8)
	now = now.Add(10*time.Minute + time.Second)
	if _, _, ok := s.Take(state, user, org); ok {
		t.Fatal("expired take")
	}
}

func TestSignInsCaps(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := NewSignIns[int](10*time.Minute, func() time.Time { return now })
	user, org := uuid.New(), uuid.New()
	var states []string
	for i := range maxSignInsPerUser + 1 {
		now = now.Add(time.Second)
		state, _, err := s.Start(user, org, i)
		if err != nil {
			t.Fatal(err)
		}
		states = append(states, state)
	}
	if _, _, ok := s.Take(states[0], user, org); ok {
		t.Fatal("oldest entry of the user was not evicted")
	}
	if _, v, ok := s.Take(states[1], user, org); !ok || v != 1 {
		t.Fatalf("second entry: %v %v", v, ok)
	}

	s = NewSignIns[int](10*time.Minute, func() time.Time { return now })
	for range maxSignIns {
		if _, _, err := s.Start(uuid.New(), org, 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.Start(uuid.New(), org, 0); !errors.Is(err, ErrTooManySignIns) {
		t.Fatalf("global cap: %v", err)
	}
	now = now.Add(11 * time.Minute)
	if _, _, err := s.Start(uuid.New(), org, 0); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
}

func TestNoRedirects(t *testing.T) {
	targetHit := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetHit = true }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	ctx := context.Background()
	for _, client := range []*http.Client{nil, {}} {
		if _, err := (HTTPVerifier{Client: client, GitHubAPI: srv.URL}).Verify(ctx, GitHub, SourcePAT, dummyGitHub); !errors.Is(err, ErrUnreachable) {
			t.Errorf("verify: %v", err)
		}
		if _, err := (HTTPClaudeOAuth{Client: client, TokenURL: srv.URL}).Refresh(ctx, dummyRefresh); !errors.Is(err, ErrUnreachable) {
			t.Errorf("refresh: %v", err)
		}
	}
	if targetHit {
		t.Fatal("redirect was followed")
	}
}

func TestClaudeExpiresIn(t *testing.T) {
	var expiresIn int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"expires_in":%d}`, dummyClaude, dummyRefresh, expiresIn)
	}))
	defer srv.Close()
	c := HTTPClaudeOAuth{TokenURL: srv.URL}
	for _, expiresIn = range []int64{0, -5} {
		if _, err := c.Refresh(context.Background(), dummyRefresh); !errors.Is(err, ErrUnreachable) {
			t.Errorf("expires_in %d: %v", expiresIn, err)
		}
	}
	expiresIn = 1 << 40
	tok, err := c.Refresh(context.Background(), dummyRefresh)
	if err != nil || time.Until(tok.ExpiresAt) > 366*24*time.Hour {
		t.Fatalf("clamp: %v %v", tok.ExpiresAt, err)
	}
}

func TestOAuthTokensRedactedInJSONAndLogs(t *testing.T) {
	tok := OAuthTokens{Access: dummyClaude, Refresh: dummyRefresh}
	b, err := json.Marshal(struct{ T OAuthTokens }{tok})
	if err != nil || string(b) != `{"T":"redacted"}` {
		t.Fatalf("json: %s %v", b, err)
	}
	var buf strings.Builder
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "tokens", tok)
	if strings.Contains(buf.String(), dummyClaude) || strings.Contains(buf.String(), dummyRefresh) || !strings.Contains(buf.String(), "redacted") {
		t.Fatalf("log: %s", buf.String())
	}
}

func TestResponseLimits(t *testing.T) {
	big := strings.Repeat("a", maxResponse+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			fmt.Fprintf(w, `{"login":%q}`, big)
			return
		}
		fmt.Fprintf(w, `{"access_token":%q,"expires_in":60}`, big)
	}))
	defer srv.Close()
	if _, err := (HTTPVerifier{GitHubAPI: srv.URL}).Verify(context.Background(), GitHub, SourcePAT, dummyGitHub); !errors.Is(err, ErrUnreachable) {
		t.Errorf("verify: %v", err)
	}
	if _, err := (HTTPClaudeOAuth{TokenURL: srv.URL + "/token"}).Refresh(context.Background(), dummyRefresh); !errors.Is(err, ErrUnreachable) {
		t.Errorf("refresh: %v", err)
	}
}

func TestAccountFieldsTruncated(t *testing.T) {
	long := "a" + strings.Repeat("é", 200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-OAuth-Scopes", long)
		fmt.Fprintf(w, `{"login":%q}`, long)
	}))
	defer srv.Close()
	acc, err := HTTPVerifier{GitHubAPI: srv.URL}.Verify(context.Background(), GitHub, SourcePAT, dummyGitHub)
	if err != nil || len(acc.Name) != 255 || len(acc.Scopes) != 255 || !utf8.ValidString(acc.Name) {
		t.Fatalf("got %d %d %v", len(acc.Name), len(acc.Scopes), err)
	}
}

func TestHTTPClaudeUsageRetryAfter(t *testing.T) {
	var retry string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if retry != "" {
			w.Header().Set("Retry-After", retry)
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := HTTPClaudeUsage{URL: srv.URL}
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{
		{"120", 2 * time.Minute},
		{time.Now().Add(10 * time.Minute).UTC().Format(http.TimeFormat), 10 * time.Minute},
		{"", 0},
		{"soon", 0},
		{"-5", 0},
		{time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), 0},
	} {
		retry = tc.header
		_, err := c.Usage(context.Background(), dummyClaude)
		if !errors.Is(err, ErrUnreachable) {
			t.Fatalf("Retry-After %q: %v", tc.header, err)
		}
		var ra *RetryAfterError
		got := time.Duration(0)
		if errors.As(err, &ra) {
			got = ra.After
		}
		if got < tc.want-2*time.Second || got > tc.want {
			t.Errorf("Retry-After %q: got %s, want %s", tc.header, got, tc.want)
		}
	}
}

func TestHTTPClaudeUsageParses(t *testing.T) {
	var status int
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+dummyClaude || r.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
			t.Errorf("request: %s %v", r.Method, r.Header)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	c := HTTPClaudeUsage{URL: srv.URL}
	ctx := context.Background()

	status, body = http.StatusOK, `{"five_hour":{"utilization":42.5,"resets_at":"2026-11-20T15:00:00Z"},"seven_day":{"utilization":7,"resets_at":null},"seven_day_opus":null}`
	u, err := c.Usage(ctx, dummyClaude)
	if err != nil || u.FiveHourPct == nil || *u.FiveHourPct != 42.5 || u.WeekPct == nil || *u.WeekPct != 7 ||
		u.FiveHourResetsAt == nil || !u.FiveHourResetsAt.Equal(time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)) || u.WeekResetsAt != nil {
		t.Fatalf("usage: %+v %v", u, err)
	}
	for code, want := range map[int]error{http.StatusForbidden: ErrRejected, http.StatusInternalServerError: ErrUnreachable} {
		status, body = code, `{"error":"`+dummyClaude+`"}`
		_, err := c.Usage(ctx, dummyClaude)
		if !errors.Is(err, want) || strings.Contains(err.Error(), dummyClaude) {
			t.Errorf("HTTP %d: %v", code, err)
		}
	}
}
