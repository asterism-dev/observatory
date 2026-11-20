package telemetry

import (
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// LeakingLogsQL lists LogsQL token runs (e.g. "| union", ":in(") that read past a forced extra_filters scope on VictoriaLogs v1.54.0; vlprobe_test.go keeps it honest.
var LeakingLogsQL = []string{}

// Words and single ASCII punctuation; anything else separates tokens.
var logsqlToken = regexp.MustCompile("[a-z0-9_]+|[!-/:-@\\[-`{-~]")

// DeniedLogsQL is a conservative token scan (not a parser) of every parameter value for a LeakingLogsQL construct.
func DeniedLogsQL(params url.Values) bool {
	for _, c := range LeakingLogsQL {
		for _, vs := range params {
			for _, v := range vs {
				if LogsQLContains(v, c) {
					return true
				}
			}
		}
	}
	return false
}

// LogsQLContains reports whether construct's tokens appear as a contiguous, case-insensitive run in query's tokens.
func LogsQLContains(query, construct string) bool {
	want := logsqlToken.FindAllString(strings.ToLower(construct), -1)
	if len(want) == 0 {
		return false
	}
	toks := logsqlToken.FindAllString(strings.ToLower(query), -1)
	for i := range toks {
		if slices.Equal(toks[i:min(i+len(want), len(toks))], want) {
			return true
		}
	}
	return false
}
