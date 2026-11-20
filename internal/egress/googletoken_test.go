package egress

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/asterism-dev/observatory/internal/proto"
)

const googleTokenURL = "https://oauth2.googleapis.com/token"

func googleRig(t *testing.T) *rig {
	t.Helper()
	return newRig(t, []proto.HostBoundSecret{ghSecret("*.googleapis.com")}, nil)
}

func postForm(t *testing.T, target, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest("POST", target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func refreshForm(refresh string) string {
	return url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {ghPH}, "client_secret": {ghPH}}.Encode()
}

func TestGoogleRefreshAnswered(t *testing.T) {
	r := googleRig(t)
	res, body := r.do(r.client(false), postForm(t, googleTokenURL, refreshForm(ghPH)))
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("status %d type %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	var got struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != ghPH || got.ExpiresIn != 3600 || got.TokenType != "Bearer" {
		t.Fatalf("body %s", body)
	}
	if n := len(r.requests()); n != 0 {
		t.Fatalf("upstream saw %d requests", n)
	}
}

func TestGoogleRefreshPassesForeign(t *testing.T) {
	r := googleRig(t)
	form := refreshForm("1//real-looking")
	r.do(r.client(false), postForm(t, googleTokenURL, form))
	if got := r.last().body; got != form {
		t.Fatalf("upstream body %q", got)
	}
}

func TestGoogleRefreshOtherPaths(t *testing.T) {
	r := googleRig(t)
	c := r.client(false)
	r.do(c, get(t, "https://oauth2.googleapis.com/tokeninfo"))
	form := url.Values{"grant_type": {"authorization_code"}, "refresh_token": {ghPH}}.Encode()
	r.do(c, postForm(t, googleTokenURL, form))
	got := r.requests()
	if len(got) != 2 || got[0].path != "/tokeninfo" || got[1].body != form {
		t.Fatalf("upstream saw %+v", got)
	}
}

func TestGoogleRefreshOversized(t *testing.T) {
	r := googleRig(t)
	form := refreshForm(ghPH) + "&pad=" + strings.Repeat("x", 65<<10)
	res, _ := r.do(r.client(false), postForm(t, googleTokenURL, form))
	if res.StatusCode != 200 || r.last().body != form {
		t.Fatalf("status %d, upstream body len %d want %d", res.StatusCode, len(r.last().body), len(form))
	}
}
