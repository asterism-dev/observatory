package rest

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNoQuery(t *testing.T) {
	h := NoQuery("/pkg.v1.Service/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	for _, tc := range []struct {
		target string
		want   int
	}{
		{"/orgs/a", http.StatusTeapot},
		{"/orgs/a?org=b", http.StatusBadRequest},
		{"/orgs/a?", http.StatusTeapot},
		{"/pkg.v1.Service/GetOrg?connect=v1&encoding=json&message=e30", http.StatusTeapot},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.target, nil))
		if rec.Code != tc.want {
			t.Errorf("%s: %d, want %d", tc.target, rec.Code, tc.want)
		}
	}
}
