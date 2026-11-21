package servicetoken

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const tokA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const tokB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestServiceTokensParse(t *testing.T) {
	got, err := Parse(" edge=" + tokA + ", compute=" + tokB)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["edge"] != tokA || got["compute"] != tokB {
		t.Fatalf("parsed %v", got)
	}
	if got, err := Parse(""); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	for name, env := range map[string]string{
		"short token":    "edge=short",
		"duplicate name": "edge=" + tokA + ",edge=" + tokB,
		"shared token":   "edge=" + tokA + ",compute=" + tokA,
		"no equals":      "edge" + tokA,
		"empty name":     "=" + tokA,
		"empty entry":    "edge=" + tokA + ",",
	} {
		_, err := Parse(env)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), tokA) || strings.Contains(err.Error(), tokB) {
			t.Errorf("%s: error leaks the token: %v", name, err)
		}
	}
}

func TestMiddlewareSetsCaller(t *testing.T) {
	h := Middleware(map[string]string{"edge": tokA, "compute": tokB})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, _ := Caller(r.Context())
		_, _ = w.Write([]byte(name))
	}))
	for auth, want := range map[string]string{"Bearer " + tokB: "compute", "bearer " + tokA: "edge"} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", auth)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Errorf("%s: %d %q", want, rec.Code, rec.Body.String())
		}
	}
	for _, auth := range []string{"", "Bearer ", "Bearer " + tokA + "x", "Basic " + tokA, tokA} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", auth)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%q: %d", auth, rec.Code)
		}
	}
}
