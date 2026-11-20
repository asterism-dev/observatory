package integrations

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidQuotaProject(t *testing.T) {
	for _, ok := range []string{"", "bna-trader-tf-state"} {
		if !ValidQuotaProject(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"Bad", "a", `x"y`, strings.Repeat("a", 31), "abcd", "abcde-"} {
		if ValidQuotaProject(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseADC(t *testing.T) {
	adc, err := ParseADC(`{"type":"authorized_user","client_id":"cid","client_secret":"sec","refresh_token":"rt","quota_project_id":"my-proj","extra":1}`)
	if err != nil || adc != (ADC{"cid", "sec", "rt", "my-proj"}) {
		t.Fatalf("%+v, %v", adc, err)
	}
	if got := adc.Marshal(); got != `{"client_id":"cid","client_secret":"sec","refresh_token":"rt"}` {
		t.Errorf("marshal: %s", got)
	}
	for _, s := range []string{adc.String(), adc.GoString()} {
		if strings.Contains(s, "sec") || strings.Contains(s, "cid") {
			t.Errorf("leak: %s", s)
		}
	}
	for name, raw := range map[string]string{
		"service account": `{"type":"service_account","client_id":"a","client_secret":"b","refresh_token":"c"}`,
		"missing refresh": `{"type":"authorized_user","client_id":"a","client_secret":"b"}`,
		"not json":        `nope`,
		"too big":         `{"type":"authorized_user","client_id":"a","client_secret":"b","refresh_token":"` + strings.Repeat("x", 17<<10) + `"}`,
	} {
		if _, err := ParseADC(raw); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestGoogleRefresh(t *testing.T) {
	blob := ADC{ClientID: "cid", ClientSecret: "sec", RefreshToken: "rt"}.Marshal()
	var form map[string][]string
	status, body := 200, `{"access_token":"ya29.x","expires_in":3599}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	g := HTTPGoogleOAuth{TokenURL: srv.URL}

	tok, err := g.Refresh(context.Background(), blob)
	if err != nil || tok.Access != "ya29.x" || tok.Refresh != blob {
		t.Fatalf("%+v, %v", tok, err)
	}
	if d := time.Until(tok.ExpiresAt); d < 3500*time.Second || d > 3600*time.Second {
		t.Errorf("expiry in %v", d)
	}
	if form["grant_type"][0] != "refresh_token" || form["client_id"][0] != "cid" || form["client_secret"][0] != "sec" || form["refresh_token"][0] != "rt" {
		t.Errorf("form: %v", form)
	}

	status, body = 400, `{"error":"invalid_grant","error_subtype":"invalid_rapt"}`
	if _, err := g.Refresh(context.Background(), blob); !errors.Is(err, ErrInvalidGrant) {
		t.Errorf("invalid_grant: %v", err)
	}
	status, body = 401, `{"error":"invalid_client"}`
	if _, err := g.Refresh(context.Background(), blob); !errors.Is(err, ErrRejected) {
		t.Errorf("401: %v", err)
	}
	status, body = 503, ``
	if _, err := g.Refresh(context.Background(), blob); !errors.Is(err, ErrUnreachable) {
		t.Errorf("503: %v", err)
	}
	if _, err := g.Refresh(context.Background(), "junk"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("junk blob: %v", err)
	}
}

func TestGoogleEmail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ya29.x" {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`{"email":"me@example.com"}`))
	}))
	defer srv.Close()
	g := HTTPGoogleOAuth{UserinfoURL: srv.URL}
	if got, err := g.Email(context.Background(), "ya29.x"); err != nil || got != "me@example.com" {
		t.Errorf("%q, %v", got, err)
	}
	if _, err := g.Email(context.Background(), "bad"); !errors.Is(err, ErrRejected) {
		t.Errorf("bad token: %v", err)
	}
}
