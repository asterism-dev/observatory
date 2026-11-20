package integrations

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const ClaudeUsageURL = "https://api.anthropic.com/api/oauth/usage"

// Usage is a poll result; utilization is already a 0–100 percentage here.
type Usage struct {
	FiveHourPct, WeekPct           *float64
	FiveHourResetsAt, WeekResetsAt *time.Time
}

type ClaudeUsage interface {
	Usage(ctx context.Context, access string) (Usage, error)
}

// A caller-supplied Client is used with redirects disabled, so the token never follows a redirect.
type HTTPClaudeUsage struct {
	Client *http.Client
	URL    string
}

type usageWindow struct {
	Utilization *float64   `json:"utilization"`
	ResetsAt    *time.Time `json:"resets_at"`
}

func (c HTTPClaudeUsage) Usage(ctx context.Context, access string) (Usage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, orDefault(c.URL, ClaudeUsageURL), nil)
	if err != nil {
		return Usage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	resp, err := noRedirects(c.Client).Do(req)
	if err != nil {
		return Usage{}, fmt.Errorf("%w: %s: usage request failed", ErrUnreachable, Claude)
	}
	defer resp.Body.Close()
	var out struct {
		FiveHour usageWindow `json:"five_hour"`
		SevenDay usageWindow `json:"seven_day"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 || resp.StatusCode/100 == 3:
		return Usage{}, fmt.Errorf("%w: %s: HTTP %d", ErrUnreachable, Claude, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return Usage{}, fmt.Errorf("%w: %s: HTTP %d", ErrRejected, Claude, resp.StatusCode)
	case json.Unmarshal(body, &out) != nil:
		return Usage{}, fmt.Errorf("%w: %s: malformed usage response", ErrUnreachable, Claude)
	}
	return Usage{FiveHourPct: out.FiveHour.Utilization, FiveHourResetsAt: out.FiveHour.ResetsAt,
		WeekPct: out.SevenDay.Utilization, WeekResetsAt: out.SevenDay.ResetsAt}, nil
}
