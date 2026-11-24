// Package policy decides allow/deny for account-owned resources from a caller's resolved rights.
package policy

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// ID names a resource: obs:<service>:<account>:<type>/<id>[/<type>/<id>…].
type ID struct {
	Service string
	Account uuid.UUID
	Path    string
}

var (
	serviceRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	segmentRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

func ParseID(s string) (ID, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 4 || parts[0] != "obs" {
		return ID{}, fmt.Errorf("invalid identifier %q", s)
	}
	if !serviceRe.MatchString(parts[1]) {
		return ID{}, fmt.Errorf("invalid service in %q", s)
	}
	acc, err := uuid.Parse(parts[2])
	// Only the canonical form, so one account never has two spellings a pattern could tell apart.
	if err != nil || acc.String() != parts[2] {
		return ID{}, fmt.Errorf("invalid account in %q", s)
	}
	if err := validPath(parts[3]); err != nil {
		return ID{}, fmt.Errorf("%w in %q", err, s)
	}
	return ID{Service: parts[1], Account: acc, Path: parts[3]}, nil
}

func validPath(p string) error {
	segs := strings.Split(p, "/")
	if len(segs)%2 != 0 {
		return errors.New("path needs <type>/<id> pairs")
	}
	for _, seg := range segs {
		if !segmentRe.MatchString(seg) {
			return fmt.Errorf("invalid path segment %q", seg)
		}
	}
	return nil
}

func (id ID) String() string {
	return "obs:" + id.Service + ":" + id.Account.String() + ":" + id.Path
}

func NewID(service string, account uuid.UUID, path ...string) ID {
	return ID{Service: service, Account: account, Path: strings.Join(path, "/")}
}
