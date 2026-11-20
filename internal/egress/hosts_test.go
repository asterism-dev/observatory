package egress

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestNormalizeHosts(t *testing.T) {
	got, err := NormalizeHosts([]string{"API.GitHub.com", "github.com", "github.com"})
	if err != nil || !reflect.DeepEqual(got, []string{"api.github.com", "github.com"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, err := NormalizeHosts(nil); got != nil || err != nil {
		t.Fatalf("nil in: %v, %v", got, err)
	}
	eleven := make([]string, 11)
	for i := range eleven {
		eleven[i] = fmt.Sprintf("h%d.test", i)
	}
	bad := [][]string{
		{"*.github.com"}, {"1.2.3.4"}, {"::1"}, {"github.com:443"}, {"localhost"}, {"a..b"}, {"127.1"}, {"1.2.3.04"}, {"010.0.0.1"}, {"0x7f.1"}, {"a.0x1f"}, {"a.123"},
		{strings.Repeat("a", 64) + ".test"}, eleven,
	}
	for _, in := range bad {
		if _, err := NormalizeHosts(in); !errors.Is(err, ErrInvalidHost) {
			t.Errorf("%v: err = %v", in, err)
		}
	}
}

func TestNormalizeSNI(t *testing.T) {
	if got := NormalizeSNI("A.Test."); got != "a.test" {
		t.Fatal(got)
	}
}

func TestNewPlaceholder(t *testing.T) {
	a, err := NewPlaceholder()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewPlaceholder()
	if !regexp.MustCompile(`^obsph_[a-z2-7]{32}$`).MatchString(a) || a == b {
		t.Fatalf("%q %q", a, b)
	}
}

func TestNormalizeHostSet(t *testing.T) {
	hosts := make([]string, MaxCAHosts+1)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("h%d.test", i)
	}
	if got, err := NormalizeHostSet(hosts[:MaxCAHosts]); err != nil || len(got) != MaxCAHosts {
		t.Fatalf("%d hosts: %d, %v", MaxCAHosts, len(got), err)
	}
	if _, err := NormalizeHostSet(hosts); !errors.Is(err, ErrInvalidHost) {
		t.Fatalf("%d hosts: err = %v", len(hosts), err)
	}
	if _, err := NewCA(hosts, time.Now()); !errors.Is(err, ErrInvalidHost) {
		t.Fatalf("NewCA with %d hosts: err = %v", len(hosts), err)
	}
}

func TestHostMatches(t *testing.T) {
	for _, c := range []struct {
		p, h string
		want bool
	}{
		{"*.googleapis.com", "logging.googleapis.com", true},
		{"*.googleapis.com", "us-central1-aiplatform.googleapis.com", true},
		{"*.googleapis.com", "a.b.googleapis.com", true},
		{"*.googleapis.com", "googleapis.com", false},
		{"*.googleapis.com", "xgoogleapis.com", false},
		{"*.googleapis.com", "googleapis.com.evil.test", false},
		{"api.github.com", "api.github.com", true},
		{"api.github.com", "x.api.github.com", false},
	} {
		if got := HostMatches(c.p, c.h); got != c.want {
			t.Errorf("HostMatches(%q, %q) = %v", c.p, c.h, got)
		}
	}
}

func TestWildcardOnlyInHostSet(t *testing.T) {
	if _, err := NormalizeHosts([]string{"*.googleapis.com"}); !errors.Is(err, ErrInvalidHost) {
		t.Fatal(err)
	}
	if _, err := NormalizeHostSet([]string{"*.googleapis.com"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"*.com", "*", "a.*.com", "**.googleapis.com", "*.1.2"} {
		if _, err := NormalizeHostSet([]string{bad}); !errors.Is(err, ErrInvalidHost) {
			t.Errorf("%q accepted", bad)
		}
	}
}
