package egress

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

const (
	claudePH  = "obsph_cccccccccccccccccccccccccccccccc"
	tokA      = "tok-account-a-0123456789"
	tokB      = "tok-account-b-9876543210"
	tokC      = "tok-account-c-5555555555"
	claudeIID = "0f6c1d2e-3a4b-4c5d-8e9f-001122334455"
)

func claudeSecret(alts ...proto.AlternateValue) proto.HostBoundSecret {
	return proto.HostBoundSecret{ID: "integration:" + claudeIID, Name: "CLAUDE_CODE_OAUTH_TOKEN", Placeholder: claudePH,
		Value: tokA, Account: "acc-a", Hosts: []string{claudeHost}, Alternates: alts}
}

// fakeClaude answers 429 with a confirmed limit for the values in limited, 200 otherwise.
type fakeClaude struct {
	mu      sync.Mutex
	limited map[string]bool
	plain   bool   // a 429 without unified headers instead
	echo    string // written into 200 bodies
	gate    func(tok string)
	auths   []string
	bodies  []string
}

func (f *fakeClaude) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	tok := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	f.auths = append(f.auths, tok)
	f.bodies = append(f.bodies, string(b))
	n, limited, plain, gate := len(f.auths), f.limited[tok], f.plain, f.gate
	f.mu.Unlock()
	if gate != nil {
		gate(tok)
	}
	h := w.Header()
	if limited && plain {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintf(w, "slow down %d", n)
		return
	}
	if limited {
		h.Set(limitHeaderPrefix+"status", "rejected")
		h.Set(limitHeaderPrefix+"reset", itoa(time.Now().Add(time.Hour).Unix()))
		h.Set(limitHeaderPrefix+"5h-utilization", "1.0")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintf(w, "rejected %d", n)
		return
	}
	h.Set(limitHeaderPrefix+"status", "allowed")
	h.Set(limitHeaderPrefix+"5h-utilization", "0.2")
	fmt.Fprintf(w, "ok %d%s", n, f.echo)
}

func (f *fakeClaude) seen() (auths, bodies []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auths...), append([]string(nil), f.bodies...)
}

func (f *fakeClaude) setLimited(toks ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limited = map[string]bool{}
	for _, t := range toks {
		f.limited[t] = true
	}
}

type reports struct {
	mu  sync.Mutex
	got []proto.UsageObservation
}

func (rs *reports) opt(r *rig) {
	r.p.Report = func(o proto.UsageObservation) {
		rs.mu.Lock()
		rs.got = append(rs.got, o)
		rs.mu.Unlock()
	}
}

func (rs *reports) all() []proto.UsageObservation {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([]proto.UsageObservation(nil), rs.got...)
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func claudePost(t *testing.T, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest("POST", "https://"+claudeHost+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+claudePH)
	return req
}

func TestRetryNextAccountOnConfirmedLimit(t *testing.T) {
	f := &fakeClaude{}
	f.setLimited(tokA)
	var rs reports
	r := newRig(t, []proto.HostBoundSecret{claudeSecret(proto.AlternateValue{Account: "acc-b", Value: tokB})}, f, rs.opt)
	res, body := r.do(r.client(false), claudePost(t, `{"model":"m"}`))
	if res.StatusCode != 200 || body != "ok 2" {
		t.Fatalf("status %d body %q", res.StatusCode, body)
	}
	auths, bodies := f.seen()
	if len(auths) != 2 || auths[0] != tokA || auths[1] != tokB {
		t.Fatalf("upstream saw %q", auths)
	}
	if bodies[0] != `{"model":"m"}` || bodies[1] != bodies[0] {
		t.Fatalf("bodies %q", bodies)
	}
	got := rs.all()
	if len(got) != 2 || got[0].Account != "acc-a" || got[0].Integration != claudeIID || got[0].LimitedUntil == nil ||
		got[1].Account != "acc-b" || got[1].LimitedUntil != nil {
		t.Fatalf("reports %+v", got)
	}
}

func TestRetryChainExhausted(t *testing.T) {
	f := &fakeClaude{}
	f.setLimited(tokA, tokB)
	r := newRig(t, []proto.HostBoundSecret{claudeSecret(proto.AlternateValue{Account: "acc-b", Value: tokB})}, f)
	res, body := r.do(r.client(false), claudePost(t, "x"))
	if res.StatusCode != 429 || body != "rejected 2" {
		t.Fatalf("status %d body %q", res.StatusCode, body)
	}
}

func TestRetryChainSkipsToThird(t *testing.T) {
	f := &fakeClaude{}
	f.setLimited(tokA, tokB)
	r := newRig(t, []proto.HostBoundSecret{claudeSecret(
		proto.AlternateValue{Account: "acc-b", Value: tokB}, proto.AlternateValue{Account: "acc-c", Value: tokC})}, f)
	res, body := r.do(r.client(true), claudePost(t, "x"))
	if auths, _ := f.seen(); res.StatusCode != 200 || body != "ok 3" || len(auths) != 3 || auths[2] != tokC {
		t.Fatalf("status %d body %q auths %q", res.StatusCode, body, auths)
	}
}

func TestNoRetryWithoutAlternates(t *testing.T) {
	f := &fakeClaude{}
	f.setLimited(tokA)
	var rs reports
	r := newRig(t, []proto.HostBoundSecret{claudeSecret()}, f, rs.opt)
	res, _ := r.do(r.client(false), claudePost(t, "x"))
	if auths, _ := f.seen(); res.StatusCode != 429 || len(auths) != 1 {
		t.Fatalf("status %d auths %q", res.StatusCode, auths)
	}
	if got := rs.all(); len(got) != 1 || got[0].Account != "acc-a" || got[0].LimitedUntil == nil {
		t.Fatalf("reports %+v", got)
	}
}

func TestNoRetryOverBodyCap(t *testing.T) {
	f := &fakeClaude{}
	f.setLimited(tokA)
	r := newRig(t, []proto.HostBoundSecret{claudeSecret(proto.AlternateValue{Account: "acc-b", Value: tokB})}, f)
	c := r.client(false)
	c.Timeout = time.Minute
	res, _ := r.do(c, claudePost(t, strings.Repeat("a", maxRetryBody+1)))
	auths, bodies := f.seen()
	if res.StatusCode != 429 || len(auths) != 1 || len(bodies[0]) != maxRetryBody+1 {
		t.Fatalf("status %d auths %d", res.StatusCode, len(auths))
	}
}

func TestRetryBufferBudget(t *testing.T) {
	defer func(old int64) { retryBudget = old }(retryBudget)
	retryBudget = 100
	f := &fakeClaude{}
	f.setLimited(tokA)
	firstAtUpstream, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	f.gate = func(string) {
		if calls.Add(1) == 1 {
			close(firstAtUpstream)
			<-release
		}
	}
	r := newRig(t, []proto.HostBoundSecret{claudeSecret(proto.AlternateValue{Account: "acc-b", Value: tokB})}, f)
	first := make(chan int)
	go func() {
		res, err := r.client(false).Do(claudePost(t, strings.Repeat("1", 60)))
		if err != nil {
			t.Error(err)
			first <- 0
			return
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		first <- res.StatusCode
	}()
	<-firstAtUpstream
	res, _ := r.do(r.client(false), claudePost(t, strings.Repeat("2", 60)))
	close(release)
	if code := <-first; code != 200 || res.StatusCode != 429 {
		t.Fatalf("first %d second %d", code, res.StatusCode)
	}
	auths, bodies := f.seen()
	var second int
	for _, b := range bodies {
		if strings.HasPrefix(b, "2") {
			second++
		}
	}
	if len(auths) != 3 || second != 1 {
		t.Fatalf("upstream calls %d, second request %d", len(auths), second)
	}
	if n := r.p.retryBuffered.Load(); n != 0 {
		t.Fatalf("budget still holds %d bytes", n)
	}
}

func TestNoRetryOnPlain429(t *testing.T) {
	f := &fakeClaude{plain: true}
	f.setLimited(tokA)
	r := newRig(t, []proto.HostBoundSecret{claudeSecret(proto.AlternateValue{Account: "acc-b", Value: tokB})}, f)
	res, body := r.do(r.client(false), claudePost(t, "x"))
	if auths, _ := f.seen(); res.StatusCode != 429 || body != "slow down 1" || len(auths) != 1 {
		t.Fatalf("status %d body %q auths %q", res.StatusCode, body, auths)
	}
}

func TestRetryConcurrentLimit(t *testing.T) {
	f := &fakeClaude{}
	f.setLimited(tokA)
	arrived := make(chan struct{}, 2)
	f.gate = func(tok string) {
		if tok != tokA {
			return
		}
		// Both requests must reach account A before either learns it is limited.
		arrived <- struct{}{}
		for len(arrived) < 2 {
			time.Sleep(time.Millisecond)
		}
	}
	var logs lockedBuf
	r := newRig(t, []proto.HostBoundSecret{claudeSecret(proto.AlternateValue{Account: "acc-b", Value: tokB})}, f,
		func(r *rig) { r.p.Log = log.New(&logs, "", 0) })
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := r.client(false).Do(claudePost(t, "x"))
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			codes[i] = res.StatusCode
		}()
	}
	wg.Wait()
	if codes[0] != 200 || codes[1] != 200 {
		t.Fatalf("codes %v", codes)
	}
	if n := strings.Count(logs.String(), "account=acc-a limited"); n != 1 {
		t.Fatalf("limited logged %d times:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), tokA) || strings.Contains(logs.String(), claudePH) {
		t.Fatalf("log leaks a value:\n%s", logs.String())
	}
}

func TestTableSwapResetsLocalLimits(t *testing.T) {
	f := &fakeClaude{}
	f.setLimited(tokA)
	r := newRig(t, []proto.HostBoundSecret{claudeSecret(proto.AlternateValue{Account: "acc-b", Value: tokB})}, f)
	c := r.client(false)
	r.do(c, claudePost(t, "x"))
	f.setLimited()
	r.do(c, claudePost(t, "x"))
	if auths, _ := f.seen(); len(auths) != 3 || auths[2] != tokB {
		t.Fatalf("locally limited A was used: %q", auths)
	}
	if err := r.p.SetTable(r.egress); err != nil {
		t.Fatal(err)
	}
	r.do(c, claudePost(t, "x"))
	if auths, _ := f.seen(); len(auths) != 4 || auths[3] != tokA {
		t.Fatalf("after swap upstream saw %q", auths)
	}
}

func TestAlternateValuesScrubbed(t *testing.T) {
	f := &fakeClaude{echo: " " + tokB}
	r := newRig(t, []proto.HostBoundSecret{claudeSecret(proto.AlternateValue{Account: "acc-b", Value: tokB})}, f)
	_, body := r.do(r.client(false), claudePost(t, "x"))
	if strings.Contains(body, tokB) || !strings.HasPrefix(body, "ok 1 ***") {
		t.Fatalf("body %q", body)
	}
}
