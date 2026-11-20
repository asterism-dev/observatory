package egress

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"

	"github.com/asterism-dev/observatory/internal/proto"
)

const (
	googleTokenHost = "oauth2.googleapis.com"
	googleBodyLimit = 64 << 10
)

type googleToken struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
}

// answerGoogleRefresh serves the VM's placeholder as the access token so the client sends it on to *.googleapis.com, where it is substituted.
func answerGoogleRefresh(w http.ResponseWriter, r *http.Request, secrets []proto.HostBoundSecret) bool {
	if r.Method != http.MethodPost || r.URL.Path != "/token" || len(secrets) == 0 {
		return false
	}
	read, _ := io.ReadAll(io.LimitReader(r.Body, googleBodyLimit+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(read), r.Body), r.Body}
	if len(read) > googleBodyLimit {
		return false
	}
	form, err := url.ParseQuery(string(read))
	if err != nil || form.Get("grant_type") != "refresh_token" {
		return false
	}
	refresh := form.Get("refresh_token")
	for _, s := range secrets {
		if s.Placeholder != "" && refresh == s.Placeholder {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(googleToken{
				AccessToken: s.Placeholder,
				ExpiresIn:   3600,
				TokenType:   "Bearer",
				Scope:       "openid https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/cloud-platform",
			})
			return true
		}
	}
	return false
}
