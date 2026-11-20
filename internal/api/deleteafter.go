package api

import (
	"errors"
	"regexp"
	"strconv"
	"time"
)

const minDeleteAfter = 5 * time.Minute

var deleteAfterPattern = regexp.MustCompile(`^([1-9][0-9]{0,5})([mhd])$`)

var errDeleteAfter = errors.New("deleteAfter must be a duration like 30m, 1h or 2d, or an RFC 3339 time with offset, at least 5 minutes ahead")

func parseDeleteAfter(s string, now time.Time) (time.Time, error) {
	var at time.Time
	if m := deleteAfterPattern.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := map[string]time.Duration{"m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
		at = now.Add(time.Duration(n) * unit)
	} else if t, err := time.Parse(time.RFC3339, s); err == nil {
		at = t
	} else {
		return time.Time{}, errDeleteAfter
	}
	if at.Before(now.Add(minDeleteAfter)) {
		return time.Time{}, errDeleteAfter
	}
	return at, nil
}
