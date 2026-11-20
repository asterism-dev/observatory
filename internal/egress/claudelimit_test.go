package egress

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestConfirmedLimit(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	reset := func(d time.Duration) string { return itoa(now.Add(d).Unix()) }
	rej := limitHeaderPrefix + "status"
	ovr := limitHeaderPrefix + "overage-status"
	rst := limitHeaderPrefix + "reset"
	tests := []struct {
		name   string
		status int
		h      http.Header
		ok     bool
		until  time.Time
	}{
		{"rejected", 429, hdr(rej, "rejected", rst, reset(time.Hour)), true, now.Add(time.Hour)},
		{"overage allowed", 429, hdr(rej, "rejected", ovr, "allowed", rst, reset(time.Hour)), false, time.Time{}},
		{"no headers", 429, hdr(), false, time.Time{}},
		{"200 rejected", 200, hdr(rej, "rejected", rst, reset(time.Hour)), false, time.Time{}},
		{"past reset", 429, hdr(rej, "rejected", rst, reset(-time.Hour)), true, now.Add(5 * time.Second)},
		{"far reset", 429, hdr(rej, "rejected", rst, reset(30*24*time.Hour)), true, now.Add(7 * 24 * time.Hour)},
		{"retry-after", 429, hdr(rej, "rejected", "Retry-After", "60"), true, now.Add(60 * time.Second)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			until, ok := confirmedLimit(tt.status, tt.h, now)
			if ok != tt.ok || !until.Equal(tt.until) {
				t.Fatalf("got %v %v, want %v %v", until, ok, tt.until, tt.ok)
			}
		})
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestNoteUsageOn200NoLimit(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	h := hdr(limitHeaderPrefix+"status", "allowed", limitHeaderPrefix+"5h-utilization", "0.99",
		limitHeaderPrefix+"5h-reset", "1800003600", limitHeaderPrefix+"7d-utilization", "0.5")
	u := noteUsage(h, now)
	if u == nil || u.FiveHourPct == nil || *u.FiveHourPct != 99 || *u.WeekPct != 50 {
		t.Fatalf("got %+v", u)
	}
	if u.LimitedUntil != nil || u.FiveHourResetsAt == nil || u.FiveHourResetsAt.Unix() != 1800003600 || !u.ObservedAt.Equal(now) {
		t.Fatalf("got %+v", u)
	}
}

func TestNoteUsageWithoutStatusIgnored(t *testing.T) {
	if u := noteUsage(hdr(limitHeaderPrefix+"5h-utilization", "0.5"), time.Now()); u != nil {
		t.Fatalf("got %+v", u)
	}
}
