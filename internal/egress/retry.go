package egress

import (
	"bytes"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

const (
	claudeHost   = "api.anthropic.com"
	maxRetryBody = 32 << 20
	// A 429 body is a short JSON error; the bound keeps a hostile one from filling memory.
	maxHeldBody = 1 << 20
)

// retryBudget caps the request bodies all of a proxy's Claude requests hold for a retry; a var so tests can shrink it.
var retryBudget int64 = 256 << 20

// accountTransport reports Claude usage per response and, if retry is set, resends a limit-rejected request with the next account.
type accountTransport struct {
	base    http.RoundTripper
	p       *Proxy
	t       *table
	host    string
	secret  proto.HostBoundSecret
	secrets []proto.HostBoundSecret
	// retry means the handler left the placeholders in place, so each attempt substitutes its own account's value.
	retry bool
}

// newAccountTransport returns nil unless the request carries the placeholder of a Claude secret with an account.
func (p *Proxy) newAccountTransport(r *http.Request, host string, t *table) *accountTransport {
	secrets := t.secretsFor(host)
	for _, s := range secrets {
		if s.Account == "" || s.Placeholder == "" || !carries(r.Header, s) {
			continue
		}
		upgrade := r.Header.Get("Upgrade") != "" || r.Method == http.MethodConnect
		retry := len(s.Alternates) > 0 && host == claudeHost && !upgrade
		return &accountTransport{p: p, t: t, host: host, secret: s, secrets: secrets, retry: retry}
	}
	return nil
}

func carries(h http.Header, s proto.HostBoundSecret) bool {
	for k, vs := range h {
		if allowedIn(s, k) && slices.ContainsFunc(vs, func(v string) bool { return strings.Contains(v, s.Placeholder) }) {
			return true
		}
	}
	return false
}

func (a *accountTransport) candidates() []proto.AlternateValue {
	return append([]proto.AlternateValue{{Account: a.secret.Account, Value: a.secret.Value}}, a.secret.Alternates...)
}

// sentValues returns what every candidate's substitution would send, so all of them are scrubbed, without touching h.
func (a *accountTransport) sentValues(h http.Header) (names, sent []string) {
	for i, c := range a.candidates() {
		n, s := substitute(h.Clone(), a.host, a.withValue(c.Value))
		if i == 0 {
			names = n
		}
		sent = append(sent, s...)
	}
	return names, sent
}

func (a *accountTransport) withValue(v string) []proto.HostBoundSecret {
	out := slices.Clone(a.secrets)
	for i := range out {
		if out[i].ID == a.secret.ID {
			out[i].Value = v
		}
	}
	return out
}

func (a *accountTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !a.retry {
		res, err := a.base.RoundTrip(req)
		if err == nil {
			a.observe(res, a.secret.Account)
		}
		return res, err
	}
	var buf []byte
	replay := true
	if req.Body != nil {
		reserved := int64(maxRetryBody + 1)
		if req.ContentLength >= 0 && req.ContentLength <= maxRetryBody {
			reserved = req.ContentLength
		}
		if req.ContentLength > maxRetryBody || !a.p.reserveRetry(reserved) {
			replay = false
		} else {
			var err error
			buf, err = io.ReadAll(io.LimitReader(req.Body, maxRetryBody+1))
			a.p.retryBuffered.Add(int64(len(buf)) - reserved)
			defer a.p.retryBuffered.Add(-int64(len(buf)))
			if err != nil {
				req.Body.Close()
				return nil, err
			}
			replay = len(buf) <= maxRetryBody
		}
	}
	if replay && req.Body != nil {
		req.Body.Close()
	}
	cands := a.available(time.Now())
	if !replay {
		cands = cands[:1]
	}
	var held *http.Response
	for i, c := range cands {
		out := req.Clone(req.Context())
		substitute(out.Header, a.host, a.withValue(c.Value))
		switch {
		case !replay:
			out.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(buf), req.Body), req.Body}
		case req.Body != nil:
			out.Body, out.ContentLength = io.NopCloser(bytes.NewReader(buf)), int64(len(buf))
			if len(buf) == 0 {
				out.Body = http.NoBody
			}
		}
		res, err := a.base.RoundTrip(out)
		if err != nil {
			if held != nil {
				// The 429 with its reset tells the client more than a 502 would.
				return held, nil
			}
			return nil, err
		}
		if !a.observe(res, c.Account) || i == len(cands)-1 {
			return res, nil
		}
		if held, err = hold(res); err != nil {
			return nil, err
		}
	}
	return held, nil
}

func (p *Proxy) reserveRetry(n int64) bool {
	if p.retryBuffered.Add(n) > retryBudget {
		p.retryBuffered.Add(-n)
		return false
	}
	return true
}

func hold(res *http.Response) (*http.Response, error) {
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, maxHeldBody))
	if err != nil {
		return nil, err
	}
	res.Body, res.ContentLength = io.NopCloser(bytes.NewReader(b)), int64(len(b))
	res.Header.Del("Content-Length")
	return res, nil
}

// observe reports the usage in res and marks the account limited on a confirmed limit, which it returns.
func (a *accountTransport) observe(res *http.Response, account string) bool {
	now := time.Now()
	until, limited := confirmedLimit(res.StatusCode, res.Header, now)
	if limited && a.t.markLimited(account, until) {
		a.p.Log.Printf("egress vm=%s host=%s account=%s limited until=%s", a.p.VMID, a.host, account, until.UTC().Format(time.RFC3339))
	}
	if u := noteUsage(res.Header, now); u != nil && a.p.Report != nil {
		u.Integration, u.Account = strings.TrimPrefix(a.secret.ID, "integration:"), account
		if limited {
			u.LimitedUntil = &until
		}
		a.p.Report(*u)
	}
	return limited
}

// available keeps the candidates not limited locally; with none left the first is tried alone.
func (a *accountTransport) available(now time.Time) []proto.AlternateValue {
	all := a.candidates()
	a.t.limitedMu.Lock()
	defer a.t.limitedMu.Unlock()
	out := slices.DeleteFunc(slices.Clone(all), func(c proto.AlternateValue) bool { return a.t.limited[c.Account].After(now) })
	if len(out) == 0 {
		return all[:1]
	}
	return out
}

// markLimited reports whether the account was not limited before.
func (t *table) markLimited(account string, until time.Time) bool {
	t.limitedMu.Lock()
	defer t.limitedMu.Unlock()
	prev := t.limited[account]
	if t.limited == nil {
		t.limited = map[string]time.Time{}
	}
	if until.After(prev) {
		t.limited[account] = until
	}
	return !prev.After(time.Now())
}
