package main

import (
	"flag"
	"net/http"
	"strings"
)

type ref struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func pick(kind, key string, items []ref) (ref, error) {
	for _, it := range items {
		if it.ID == key {
			return it, nil
		}
	}
	var ids []string
	var found ref
	for _, it := range items {
		if it.Name == key {
			found, ids = it, append(ids, it.ID)
		}
	}
	switch len(ids) {
	case 0:
		return found, notFoundf("%s %q not found", kind, key)
	case 1:
		return found, nil
	}
	return ref{}, usagef("%s %q is ambiguous: %s; use the ID", kind, key, strings.Join(ids, ", "))
}

type scopeFlags struct{ scope, team string }

func addScopeFlags(fs *flag.FlagSet, s *scopeFlags, name string) {
	fs.StringVar(&s.scope, name, "private", "visibility: private|team|org")
	fs.StringVar(&s.team, "team", "", "team `NAME|ID` (implies team scope)")
}

func (c *cli) resolveScope(s scopeFlags) (apiScope string, teamID *string, err error) {
	scope := s.scope
	if s.team != "" && scope == "private" {
		scope = "team"
	}
	switch scope {
	case "private":
		return "user", nil, nil
	case "org":
		if s.team != "" {
			return "", nil, usagef("--team cannot be combined with org scope")
		}
		return "org", nil, nil
	case "team":
		if s.team == "" {
			return "", nil, usagef("team scope needs --team")
		}
	default:
		return "", nil, usagef("unknown scope %q: use private, team or org", s.scope)
	}
	path, err := c.orgPath("/teams")
	if err != nil {
		return "", nil, err
	}
	var teams []ref
	if err := c.call(http.MethodGet, path, nil, &teams); err != nil {
		return "", nil, err
	}
	t, err := pick("team", s.team, teams)
	if err != nil {
		return "", nil, err
	}
	return "team", &t.ID, nil
}

// ownerLabel names the owner of another member's VM or disk instead of calling it private.
func (c *cli) ownerLabel(kind, id, name string) string {
	if kind == "user" && id != c.userID {
		return name
	}
	return scopeLabel(kind, name)
}

func scopeLabel(apiScope, teamName string) string {
	switch apiScope {
	case "user":
		return "private"
	case "team":
		return "team " + teamName
	}
	return apiScope
}
