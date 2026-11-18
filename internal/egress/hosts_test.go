package egress

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
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
