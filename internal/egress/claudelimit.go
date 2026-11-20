package egress

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

const (
	limitHeaderPrefix = "anthropic-ratelimit-unified-"
	// A reset in the past must not trigger an endless retry.
	minCooldown = 5 * time.Second
	// An implausible reset far in the future must not park the account.
	maxCooldown = 7 * 24 * time.Hour
)

// confirmedLimit mirrors Claude Code: only a 429 with status "rejected" counts, and not while overage is on offer.
func confirmedLimit(status int, h http.Header, now time.Time) (until time.Time, ok bool) {
	if status != http.StatusTooManyRequests || h.Get(limitHeaderPrefix+"status") != "rejected" {
		return time.Time{}, false
	}
	if s := h.Get(limitHeaderPrefix + "overage-status"); s != "" && s != "rejected" {
		return time.Time{}, false
	}
	if sec, ok := headerSeconds(h.Get(limitHeaderPrefix + "reset")); ok {
		return clampCooldown(time.Unix(sec, 0), now), true
	}
	if sec, ok := headerSeconds(h.Get("Retry-After")); ok {
		return clampCooldown(now.Add(time.Duration(sec)*time.Second), now), true
	}
	return time.Time{}, false
}

// headerSeconds skips the HTTP-date form of Retry-After.
func headerSeconds(v string) (int64, bool) {
	sec, err := strconv.ParseInt(v, 10, 64)
	return sec, err == nil
}

func clampCooldown(until, now time.Time) time.Time {
	if until.Before(now.Add(minCooldown)) {
		return now.Add(minCooldown)
	}
	if until.After(now.Add(maxCooldown)) {
		return now.Add(maxCooldown)
	}
	return until
}

// noteUsage ignores responses without unified-status, as Claude Code does; the caller fills ids and LimitedUntil.
func noteUsage(h http.Header, now time.Time) *proto.UsageObservation {
	if h.Get(limitHeaderPrefix+"status") == "" {
		return nil
	}
	u := &proto.UsageObservation{ObservedAt: now}
	u.FiveHourPct, u.FiveHourResetsAt = window(h, "5h")
	u.WeekPct, u.WeekResetsAt = window(h, "7d")
	return u
}

func window(h http.Header, name string) (*float64, *time.Time) {
	frac, err := strconv.ParseFloat(h.Get(limitHeaderPrefix+name+"-utilization"), 64)
	// NaN and Inf would later fail json.Marshal of the report.
	if err != nil || math.IsNaN(frac) || math.IsInf(frac, 0) || frac < 0 {
		return nil, nil
	}
	pct := frac * 100
	sec, ok := headerSeconds(h.Get(limitHeaderPrefix + name + "-reset"))
	if !ok {
		return &pct, nil
	}
	reset := time.Unix(sec, 0)
	return &pct, &reset
}
