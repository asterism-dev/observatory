package textx_test

import (
	"strings"
	"testing"

	"github.com/asterism-dev/observatory/internal/platform/textx"
)

func TestSanitize(t *testing.T) {
	for in, want := range map[string]string{
		"Mozilla/5.0":            "Mozilla/5.0",
		"a\x00b\nc\x7f":          "abc",
		"bad\xffutf8":            "badutf8",
		"Grüße":                  "Grüße",
		strings.Repeat("ü", 300): strings.Repeat("ü", 256),
	} {
		if got := textx.Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}
