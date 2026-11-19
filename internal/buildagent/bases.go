package buildagent

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/asterism-dev/observatory/internal/registry"
)

const resolveTimeout = 30 * time.Second

// BaseDigest is what the hub stores to notice base image updates.
type BaseDigest struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

// platformArgs are BuildKit's automatic platform ARGs for the linux/amd64 builds the agent runs.
var platformArgs = map[string]string{
	"TARGETPLATFORM": "linux/amd64", "TARGETOS": "linux", "TARGETARCH": "amd64",
	"BUILDPLATFORM": "linux/amd64", "BUILDOS": "linux", "BUILDARCH": "amd64",
}

// FromRefs returns the images the Dockerfile's FROM lines pull; stage names, scratch and digest pins, which cannot change, are left out.
func FromRefs(dockerfile string) []string {
	args := map[string]string{}
	for k, v := range platformArgs {
		args[k] = v
	}
	stages := map[string]bool{}
	var refs []string
	seenFrom := false
	for _, line := range instructions(dockerfile) {
		fields := strings.Fields(line)
		switch strings.ToUpper(fields[0]) {
		case "ARG":
			// Only ARGs before the first FROM apply to FROM lines.
			if seenFrom {
				continue
			}
			for _, a := range fields[1:] {
				if k, v, ok := strings.Cut(a, "="); ok {
					args[k] = expand(strings.Trim(v, `"'`), args)
				}
			}
		case "FROM":
			seenFrom = true
			var rest []string
			for _, f := range fields[1:] {
				if !strings.HasPrefix(f, "--") {
					rest = append(rest, f)
				}
			}
			if len(rest) == 0 {
				continue
			}
			ref := expand(rest[0], args)
			if ref != "" && ref != "scratch" && !stages[strings.ToLower(ref)] && !strings.Contains(ref, "@") && !slices.Contains(refs, ref) {
				refs = append(refs, ref)
			}
			if len(rest) >= 3 && strings.EqualFold(rest[1], "AS") {
				stages[strings.ToLower(rest[2])] = true
			}
		}
	}
	return refs
}

// ponytail: heredoc bodies are read as instructions; a heredoc line starting with FROM would count as a base.
func instructions(src string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(src, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if head, ok := strings.CutSuffix(t, `\`); ok {
			cur.WriteString(head + " ")
			continue
		}
		cur.WriteString(t)
		out = append(out, cur.String())
		cur.Reset()
	}
	if strings.TrimSpace(cur.String()) != "" {
		out = append(out, cur.String())
	}
	return out
}

// expand substitutes $NAME, ${NAME} and the ${NAME:-word}, ${NAME-word}, ${NAME:+word} and ${NAME+word} forms.
func expand(s string, args map[string]string) string {
	return os.Expand(s, func(k string) string {
		i := strings.IndexAny(k, ":-+")
		if i <= 0 {
			return args[k]
		}
		v, set := args[k[:i]]
		op := k[i:]
		colon := strings.HasPrefix(op, ":")
		if op = strings.TrimPrefix(op, ":"); op == "" {
			return v
		}
		switch op[0] {
		case '-':
			if !set || colon && v == "" {
				return op[1:]
			}
		case '+':
			if set && (!colon || v != "") {
				return op[1:]
			}
			return ""
		}
		return v
	})
}

// resolveBases pins each base to its linux/amd64 digest; one that does not resolve is only logged, so just its updates go unnoticed.
func (a *Agent) resolveBases(ctx context.Context, log io.Writer, recipe string, hub *url.URL) []BaseDigest {
	raw, err := os.ReadFile(filepath.Join(recipe, "Dockerfile"))
	if err != nil {
		return nil
	}
	var out []BaseDigest
	for _, s := range FromRefs(string(raw)) {
		d, err := a.resolveBase(ctx, s, hub)
		if err != nil {
			fmt.Fprintf(log, "base image %s not resolved, its updates will not trigger rebuilds: %v\n", s, err)
			continue
		}
		out = append(out, BaseDigest{Ref: s, Digest: d})
	}
	return out
}

func (a *Agent) resolveBase(ctx context.Context, s string, hub *url.URL) (string, error) {
	ref, err := name.ParseReference(s)
	if err != nil {
		return "", err
	}
	var opts []remote.Option
	if ref.Context().RegistryStr() == hub.Host {
		opts = append(opts, remote.WithAuth(&authn.Basic{Username: "observatory", Password: a.Secret}))
		if hub.Scheme == "http" {
			if ref, err = name.ParseReference(s, name.Insecure); err != nil {
				return "", err
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	return registry.PlatformDigest(ctx, ref, opts...)
}
