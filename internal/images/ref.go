package images

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

type Ref struct {
	Qualifier, Name, Tag, Digest string
}

var digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func ParseRef(s string) (Ref, error) {
	var r Ref
	if i := strings.IndexByte(s, '@'); i >= 0 {
		r.Digest = s[i+1:]
		s = s[:i]
		if !digestRe.MatchString(r.Digest) {
			return Ref{}, fmt.Errorf("invalid digest %q", r.Digest)
		}
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		r.Qualifier = s[:i]
		s = s[i+1:]
		if r.Qualifier == "" {
			return Ref{}, errors.New("empty qualifier")
		}
	}
	if i := strings.IndexByte(s, ':'); i >= 0 {
		r.Tag = s[i+1:]
		s = s[:i]
		if r.Tag == "" {
			return Ref{}, errors.New("empty tag")
		}
		if r.Digest != "" {
			return Ref{}, errors.New("reference has both tag and digest")
		}
	}
	if s == "" || strings.Contains(s, "/") {
		return Ref{}, errors.New("invalid image name")
	}
	r.Name = s
	return r, nil
}
