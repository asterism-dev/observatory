package integrations

import (
	"errors"
	"regexp"
	"strings"
)

type Kind string

const (
	GitHub Kind = "github"
	Claude Kind = "claude"
	Linear Kind = "linear"
	GCloud Kind = "gcloud"
)

type Source string

const (
	SourcePAT        Source = "pat"
	SourceSetupToken Source = "setup-token"
	SourceOAuth      Source = "oauth"
	SourceAPIKey     Source = "api-key"
	SourceADC        Source = "adc"
)

type Mode string

const (
	ModeProxy Mode = "proxy"
	ModeVM    Mode = "vm"
)

type Spec struct {
	Sources        []Source
	Hosts, Headers []string
	TokenPage      string
}

// Hosts are sorted so they equal their egress.NormalizeHostSet form.
var Kinds = map[Kind]Spec{
	GitHub: {
		Sources:   []Source{SourcePAT},
		Hosts:     []string{"api.github.com", "github.com", "uploads.github.com"},
		Headers:   []string{"Authorization"},
		TokenPage: "https://github.com/settings/tokens",
	},
	Claude: {
		Sources:   []Source{SourceSetupToken, SourceOAuth},
		Hosts:     []string{"api.anthropic.com"},
		Headers:   []string{"Authorization"},
		TokenPage: "https://claude.ai/settings/claude-code",
	},
	Linear: {
		Sources:   []Source{SourceAPIKey},
		Hosts:     []string{"api.linear.app", "mcp.linear.app"},
		Headers:   []string{"Authorization"},
		TokenPage: "https://linear.app/settings/account/security",
	},
	GCloud: {
		Sources:   []Source{SourceADC},
		Hosts:     []string{"*.googleapis.com", "*.pkg.dev"},
		Headers:   []string{"Authorization"},
		TokenPage: "https://myaccount.google.com/permissions",
	},
}

var ErrInvalidToken = errors.New("invalid token")

// tokenRE is also enforced by the guest's vmm-token, so a token can't inject lines into git's credential protocol.
var tokenRE = regexp.MustCompile(`^[A-Za-z0-9._~+/=_-]+$`)

func NormalizeToken(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) < 12 || len(s) > 4096 || !tokenRE.MatchString(s) {
		return "", ErrInvalidToken
	}
	return s, nil
}
