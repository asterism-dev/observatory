package release

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/asterism-dev/observatory/internal/version"
)

func releaseServer(t *testing.T, status *atomic.Int32, tag *atomic.Value) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if s := int(status.Load()); s != 0 && s != 200 {
			w.WriteHeader(s)
			return
		}
		fmt.Fprintf(w, `{"tag_name":%q}`, tag.Load())
	}))
	t.Cleanup(srv.Close)
	return srv
}

func setVersion(t *testing.T, v string) {
	old := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = old })
}

func TestLatest(t *testing.T) {
	var status atomic.Int32
	var tag atomic.Value
	tag.Store("v0.4.0")
	srv := releaseServer(t, &status, &tag)
	got, err := Latest(t.Context(), srv.Client(), srv.URL)
	if err != nil || got != "v0.4.0" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestLatestErrors(t *testing.T) {
	var status atomic.Int32
	var tag atomic.Value
	tag.Store("v0.4.0")
	srv := releaseServer(t, &status, &tag)
	status.Store(403)
	if _, err := Latest(t.Context(), srv.Client(), srv.URL); err == nil {
		t.Error("403: want error")
	}
	status.Store(200)
	tag.Store("nightly")
	if _, err := Latest(t.Context(), srv.Client(), srv.URL); err == nil {
		t.Error("tag nightly: want error")
	}
}

func TestCheckerNewer(t *testing.T) {
	setVersion(t, "v0.3.0")
	var status atomic.Int32
	var tag atomic.Value
	tag.Store("v0.4.0")
	srv := releaseServer(t, &status, &tag)
	c := &Checker{URL: srv.URL, Client: srv.Client()}
	if got := c.Newer(); got != "" {
		t.Fatalf("before any check: %q", got)
	}
	c.check(t.Context())
	if got := c.Newer(); got != "v0.4.0" {
		t.Fatalf("after check: %q", got)
	}
	status.Store(500)
	c.check(t.Context())
	if got := c.Newer(); got != "v0.4.0" {
		t.Fatalf("failed check must keep the last tag, got %q", got)
	}
	status.Store(200)
	tag.Store("v0.3.0")
	c.check(t.Context())
	if got := c.Newer(); got != "" {
		t.Fatalf("same version: %q", got)
	}
}

func TestEnabled(t *testing.T) {
	env := func(v string) func(string) string { return func(string) string { return v } }
	setVersion(t, "dev")
	if Enabled(env("")) {
		t.Error("dev build: want disabled")
	}
	setVersion(t, "v0.3.0")
	if !Enabled(env("")) {
		t.Error("release build: want enabled")
	}
	if Enabled(env("1")) {
		t.Error("OBSERVATORY_NO_UPDATE_CHECK=1: want disabled")
	}
}
