package edge

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const validSession = "obs_valid"

// issuingHub records hub requests and answers /internal/iam/token like the hub's issuer: 403 for a wrong service token, a JWT for validSession, 401 otherwise.
func issuingHub(t *testing.T, status int) (*httptest.Server, chan seen, *atomic.Int32) {
	t.Helper()
	ch := make(chan seen, 16)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/iam/token" {
			ch <- seen{r.URL.Path, r.URL.RawPath, r.Host, r.Header.Get(HeaderClientIP), r.Header.Get(HeaderEdge), r.Header.Clone()}
			_, _ = io.WriteString(w, "hub")
			return
		}
		calls.Add(1)
		var in struct {
			SessionToken string `json:"session_token"`
		}
		if r.Header.Get("Authorization") != "Bearer "+testServiceToken {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&in) != nil || in.SessionToken != validSession {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if status != http.StatusOK {
			http.Error(w, "boom", status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "jwt-" + in.SessionToken, "expires_at": time.Now().Add(5 * time.Minute).Format(time.RFC3339)})
	}))
	t.Cleanup(srv.Close)
	return srv, ch, &calls
}

func noRequest(t *testing.T, ch chan seen, who string) {
	t.Helper()
	select {
	case s := <-ch:
		t.Fatalf("%s was reached: %+v", who, s)
	default:
	}
}

func TestEdgeAttachesJWTToServiceRoutes(t *testing.T) {
	hub, _, _ := issuingHub(t, http.StatusOK)
	svc, ch := recorder(t, "iam")
	edge := newEdge(t, hub.URL, map[string]string{"iam": svc.URL})
	req, _ := http.NewRequest("GET", edge.URL+"/api/iam/v1/x", nil)
	req.AddCookie(&http.Cookie{Name: "obs_session", Value: validSession})
	if code, body := get(t, req); code != 200 || body != "iam" {
		t.Fatalf("%d %q", code, body)
	}
	s := <-ch
	if got := s.header.Values("Authorization"); len(got) != 1 || got[0] != "Bearer jwt-"+validSession {
		t.Fatalf("service saw Authorization %q", got)
	}
	if c := s.header.Values("Cookie"); len(c) != 0 {
		t.Fatalf("service saw cookies %q", c)
	}
}

func TestEdgeStripsClientAuthorization(t *testing.T) {
	hub, _, _ := issuingHub(t, http.StatusOK)
	svc, ch := recorder(t, "iam")
	edge := newEdge(t, hub.URL, map[string]string{"iam": svc.URL})
	for name, set := range map[string]func(http.Header){
		"session bearer": func(h http.Header) { h.Set("Authorization", "Bearer "+validSession) },
		"multi value":    func(h http.Header) { h["Authorization"] = []string{"Bearer " + validSession, "Bearer forged"} },
		"hop-by-hop": func(h http.Header) {
			h.Set("Authorization", "Bearer "+validSession)
			h.Set("Connection", "Authorization")
		},
	} {
		req, _ := http.NewRequest("GET", edge.URL+"/api/iam/v1/x", nil)
		set(req.Header)
		if code, _ := get(t, req); code != 200 {
			t.Fatalf("%s: status %d", name, code)
		}
		if got := (<-ch).header.Values("Authorization"); len(got) != 1 || got[0] != "Bearer jwt-"+validSession {
			t.Fatalf("%s: service saw Authorization %q", name, got)
		}
	}
	req, _ := http.NewRequest("GET", edge.URL+"/api/iam/v1/x", nil)
	req.Header.Set("Authorization", "Bearer forged")
	if code, _ := get(t, req); code != http.StatusUnauthorized {
		t.Fatalf("forged bearer: status %d, want 401", code)
	}
	noRequest(t, ch, "service")
}

func TestEdgeUnauthenticatedServiceRoute(t *testing.T) {
	hub, _, calls := issuingHub(t, http.StatusOK)
	svc, ch := recorder(t, "iam")
	edge := newEdge(t, hub.URL, map[string]string{"iam": svc.URL})
	for name, set := range map[string]func(http.Header){
		"none":  func(http.Header) {},
		"basic": func(h http.Header) { h.Set("Authorization", "Basic "+validSession) },
	} {
		req, _ := http.NewRequest("GET", edge.URL+"/api/iam/v1/x", nil)
		set(req.Header)
		if code, _ := get(t, req); code != http.StatusUnauthorized {
			t.Fatalf("%s: status %d, want 401", name, code)
		}
	}
	noRequest(t, ch, "service")
	if n := calls.Load(); n != 0 {
		t.Fatalf("issuer called %d times without a session", n)
	}
}

func TestEdgeCachesJWT(t *testing.T) {
	hub, _, calls := issuingHub(t, http.StatusOK)
	svc, ch := recorder(t, "iam")
	edge := newEdge(t, hub.URL, map[string]string{"iam": svc.URL})
	for range 2 {
		req, _ := http.NewRequest("GET", edge.URL+"/api/iam/v1/x", nil)
		req.Header.Set("Authorization", "Bearer "+validSession)
		if code, _ := get(t, req); code != 200 {
			t.Fatalf("status %d", code)
		}
		<-ch
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("issuer called %d times, want 1", n)
	}
}

func TestEdgeHubRoutesKeepCredentials(t *testing.T) {
	hub, ch, calls := issuingHub(t, http.StatusOK)
	edge := newEdge(t, hub.URL, map[string]string{"iam": "http://127.0.0.1:1"})
	req, _ := http.NewRequest("POST", edge.URL+"/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+validSession)
	req.AddCookie(&http.Cookie{Name: "obs_session", Value: validSession})
	if code, body := get(t, req); code != 200 || body != "hub" {
		t.Fatalf("%d %q", code, body)
	}
	s := <-ch
	if s.header.Get("Authorization") != "Bearer "+validSession || s.header.Get("Cookie") != "obs_session="+validSession {
		t.Fatalf("hub saw Authorization %q, Cookie %q", s.header.Get("Authorization"), s.header.Get("Cookie"))
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("issuer called %d times for a hub route", n)
	}
}

func TestEdgeServiceCookieCSRF(t *testing.T) {
	hub, _, _ := issuingHub(t, http.StatusOK)
	svc, ch := recorder(t, "iam")
	edge := newEdge(t, hub.URL, map[string]string{"iam": svc.URL})
	req, _ := http.NewRequest("POST", edge.URL+"/api/iam/v1/x", nil)
	req.AddCookie(&http.Cookie{Name: "obs_session", Value: validSession})
	if code, _ := get(t, req); code != http.StatusForbidden {
		t.Fatalf("cookie POST without X-Observatory: status %d, want 403", code)
	}
	noRequest(t, ch, "service")
	req, _ = http.NewRequest("POST", edge.URL+"/api/iam/v1/x", nil)
	req.AddCookie(&http.Cookie{Name: "obs_session", Value: validSession})
	req.Header.Set("X-Observatory", "1")
	if code, _ := get(t, req); code != 200 {
		t.Fatalf("cookie POST with X-Observatory: status %d", code)
	}
	<-ch
	req, _ = http.NewRequest("POST", edge.URL+"/api/iam/v1/x", nil)
	req.Header.Set("Authorization", "Bearer "+validSession)
	if code, _ := get(t, req); code != 200 {
		t.Fatalf("bearer POST: status %d", code)
	}
	<-ch
}

func TestEdgeIssuerFailure(t *testing.T) {
	hub, _, _ := issuingHub(t, http.StatusInternalServerError)
	svc, ch := recorder(t, "iam")
	edge := newEdge(t, hub.URL, map[string]string{"iam": svc.URL})
	req, _ := http.NewRequest("GET", edge.URL+"/api/iam/v1/x", nil)
	req.Header.Set("Authorization", "Bearer "+validSession)
	if code, _ := get(t, req); code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", code)
	}
	noRequest(t, ch, "service")
}

func TestEdgeMisconfiguredServiceToken(t *testing.T) {
	hub, _, _ := issuingHub(t, http.StatusOK)
	svc, ch := recorder(t, "iam")
	h, err := New(Config{HubURL: hub.URL, Token: testToken, ServiceToken: "wrong-wrong-wrong-wrong-wrong-wrong", Services: map[string]string{"iam": svc.URL}}, assets)
	if err != nil {
		t.Fatal(err)
	}
	edge := httptest.NewServer(h)
	t.Cleanup(edge.Close)
	req, _ := http.NewRequest("GET", edge.URL+"/api/iam/v1/x", nil)
	req.Header.Set("Authorization", "Bearer "+validSession)
	if code, _ := get(t, req); code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502 for a rejected service token", code)
	}
	noRequest(t, ch, "service")
}

func TestCacheUntilCapsLifetime(t *testing.T) {
	now := time.Now()
	if got := cacheUntil(now.Add(5*time.Minute), now); !got.Equal(now.Add(jwtCacheTTL)) {
		t.Fatalf("long-lived JWT cached until %v, want now+%v", got.Sub(now), jwtCacheTTL)
	}
	if got := cacheUntil(now.Add(90*time.Second), now); !got.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("short-lived JWT cached until %v, want exp-%v", got.Sub(now), jwtRefreshMargin)
	}
}

func TestJWTCacheEvictsAndExpires(t *testing.T) {
	now := time.Now()
	c := newJWTCache(2)
	a, b, d := [32]byte{1}, [32]byte{2}, [32]byte{3}
	c.add(a, "a", now.Add(time.Minute))
	c.add(b, "b", now.Add(time.Minute))
	c.get(a, now) // a becomes most recent, so b is evicted next
	c.add(d, "d", now.Add(time.Minute))
	if _, ok := c.get(b, now); ok {
		t.Fatal("least recently used entry not evicted")
	}
	if v, ok := c.get(a, now); !ok || v != "a" {
		t.Fatalf("a: %q %v", v, ok)
	}
	if _, ok := c.get(d, now.Add(time.Minute)); ok {
		t.Fatal("expired entry returned")
	}
}
