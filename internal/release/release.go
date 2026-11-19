// Package release looks up the latest observatory release on GitHub.
package release

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/asterism-dev/observatory/internal/version"
)

const LatestURL = "https://api.github.com/repos/asterism-dev/observatory/releases/latest"

// Enabled is false for dev builds and when OBSERVATORY_NO_UPDATE_CHECK=1 (air-gapped, CI).
func Enabled(getenv func(string) string) bool {
	return version.Released(version.Version) && getenv("OBSERVATORY_NO_UPDATE_CHECK") != "1"
}

func Latest(ctx context.Context, c *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("latest release: %s", resp.Status)
	}
	var body struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("latest release: %w", err)
	}
	if !version.Released(body.Tag) {
		return "", fmt.Errorf("latest release: unexpected tag %q", body.Tag)
	}
	return body.Tag, nil
}

// Checker keeps the latest release tag; a failed check keeps the previous one.
type Checker struct {
	URL    string
	Client *http.Client
	tag    atomic.Value
}

func (c *Checker) Run(ctx context.Context, every time.Duration) {
	for {
		c.check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func (c *Checker) check(ctx context.Context) {
	tag, err := Latest(ctx, c.Client, c.URL)
	if err != nil {
		slog.Warn("release check failed", "err", err)
		return
	}
	c.tag.Store(tag)
}

// Newer is the latest release if it is newer than this build, else "".
func (c *Checker) Newer() string {
	tag, _ := c.tag.Load().(string)
	if version.Older(version.Version, tag) {
		return tag
	}
	return ""
}
