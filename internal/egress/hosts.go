package egress

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"
)

const (
	PlaceholderPrefix = "obsph_"
	MinValueLen       = 12
	MaxHosts          = 10
)

var ErrInvalidHost = errors.New("invalid host")

var labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func NormalizeHosts(in []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, h := range in {
		h = strings.ToLower(strings.TrimSpace(h))
		if err := validHost(h); err != nil {
			return nil, err
		}
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	if len(out) > MaxHosts {
		return nil, fmt.Errorf("%w: at most %d hosts", ErrInvalidHost, MaxHosts)
	}
	sort.Strings(out)
	return out, nil
}

func validHost(h string) error {
	bad := func() error { return fmt.Errorf("%w: %q", ErrInvalidHost, h) }
	if len(h) > 253 {
		return bad()
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return bad()
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return bad()
	}
	for _, l := range labels {
		if !labelRE.MatchString(l) {
			return bad()
		}
	}
	return nil
}

func NormalizeSNI(s string) string {
	return strings.TrimSuffix(strings.ToLower(s), ".")
}

func NewPlaceholder() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)
	return PlaceholderPrefix + strings.ToLower(enc.EncodeToString(b)), nil
}
