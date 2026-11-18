package egress

import (
	"encoding/base64"
	"net/http"
	"slices"
	"strings"

	"github.com/asterism-dev/observatory/internal/proto"
)

const basicPrefix = "Basic "

// substitute replaces placeholders of secrets bound to host in header values; sent holds every rewritten value as sent.
func substitute(h http.Header, host string, secrets []proto.HostBoundSecret) (names, sent []string) {
	used := map[string]bool{}
	for k, vs := range h {
		var apply []proto.HostBoundSecret
		for _, s := range secrets {
			if s.Placeholder != "" && boundTo(s, host) && allowedIn(s, k) {
				apply = append(apply, s)
			}
		}
		if len(apply) == 0 {
			continue
		}
		for i, v := range vs {
			if cred, ok := basicCredentials(k, v); ok {
				if dec, err := base64.StdEncoding.DecodeString(cred); err == nil {
					if out, changed := replacePlaceholders(string(dec), apply, used); changed {
						enc := base64.StdEncoding.EncodeToString([]byte(out))
						vs[i] = v[:len(basicPrefix)] + enc
						sent = append(sent, enc)
					}
					continue
				}
			}
			if out, changed := replacePlaceholders(v, apply, used); changed {
				vs[i] = out
				sent = append(sent, out)
			}
		}
	}
	for n := range used {
		names = append(names, n)
	}
	slices.Sort(names)
	return names, sent
}

func boundTo(s proto.HostBoundSecret, host string) bool {
	return slices.ContainsFunc(s.Hosts, func(h string) bool { return NormalizeSNI(h) == host })
}

func allowedIn(s proto.HostBoundSecret, header string) bool {
	return len(s.Headers) == 0 || slices.ContainsFunc(s.Headers, func(h string) bool { return strings.EqualFold(h, header) })
}

func basicCredentials(header, v string) (string, bool) {
	if !strings.EqualFold(header, "Authorization") || len(v) < len(basicPrefix) || !strings.EqualFold(v[:len(basicPrefix)], basicPrefix) {
		return "", false
	}
	return v[len(basicPrefix):], true
}

func replacePlaceholders(v string, secrets []proto.HostBoundSecret, used map[string]bool) (string, bool) {
	changed := false
	for _, s := range secrets {
		if strings.Contains(v, s.Placeholder) {
			v = strings.ReplaceAll(v, s.Placeholder, s.Value)
			used[s.Name] = true
			changed = true
		}
	}
	return v, changed
}
