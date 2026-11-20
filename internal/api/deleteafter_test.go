package api

import (
	"testing"
	"time"
)

func TestParseDeleteAfter(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"30m":                       now.Add(30 * time.Minute),
		"1h":                        now.Add(time.Hour),
		"2d":                        now.Add(48 * time.Hour),
		"5m":                        now.Add(5 * time.Minute),
		"2026-11-21T08:00:00+01:00": time.Date(2026, 11, 21, 7, 0, 0, 0, time.UTC),
	} {
		got, err := parseDeleteAfter(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "0m", "4m", "-1h", "1.5h", "2w", "h", "2026-11-21T08:00:00", "2026-11-20T10:04:00Z"} {
		if got, err := parseDeleteAfter(in, now); err == nil {
			t.Errorf("%q accepted: %v", in, got)
		}
	}
}
