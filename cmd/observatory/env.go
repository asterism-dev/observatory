package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type envVarJSON struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Scope     string    `json:"scope"`
	TeamID    string    `json:"teamId,omitempty"`
	TeamName  string    `json:"teamName,omitempty"`
	Value     *string   `json:"value,omitempty"`
	Hosts     []string  `json:"hosts"`
	UpdatedAt time.Time `json:"updatedAt"`
	UpdatedBy ref       `json:"updatedBy"`
}

func (v envVarJSON) scopeLabel() string { return scopeLabel(v.Scope, v.TeamName) }

func varCommands() []command {
	return []command{
		{"var list", "[--scope private|team|org] [--team T]", "list variables and secrets", cmdVarList},
		{"var set", "<name> [--secret] [--host H]... [--scope S] [--team T] [--value V | --from-file F]", "create or update a variable (secrets from stdin or --from-file)", cmdVarSet},
		{"var move", "<name> [--scope S] [--team T] --to private|team|org [--to-team T]", "move a variable to another scope", cmdVarMove},
		{"var delete", "<name> [--scope S] [--team T]", "delete a variable", cmdVarDelete},
	}
}

func (c *cli) varList() ([]envVarJSON, json.RawMessage, error) {
	path, err := c.orgPath("/env")
	if err != nil {
		return nil, nil, err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodGet, path, nil, &raw); err != nil {
		return nil, nil, err
	}
	var l struct{ Vars []envVarJSON }
	return l.Vars, raw, json.Unmarshal(raw, &l)
}

func matchesScope(v envVarJSON, apiScope string, teamID *string) bool {
	return v.Scope == apiScope && (teamID == nil || v.TeamID == *teamID)
}

func (c *cli) lookupVar(name, apiScope string, teamID *string) (envVarJSON, bool, error) {
	l, _, err := c.varList()
	if err != nil {
		return envVarJSON{}, false, err
	}
	for _, v := range l {
		if v.Name == name && matchesScope(v, apiScope, teamID) {
			return v, true, nil
		}
	}
	return envVarJSON{}, false, nil
}

func (c *cli) findVar(name string, s scopeFlags) (envVarJSON, error) {
	apiScope, teamID, err := c.resolveScope(s)
	if err != nil {
		return envVarJSON{}, err
	}
	v, ok, err := c.lookupVar(name, apiScope, teamID)
	if err == nil && !ok {
		label := scopeLabel(apiScope, s.team)
		err = notFoundf("variable %s not found (%s)", name, label)
	}
	return v, err
}

func varValueCell(v envVarJSON) string {
	val := "••••••"
	if v.Value != nil {
		val = *v.Value
	}
	if len(v.Hosts) > 0 {
		val += " → " + strings.Join(v.Hosts, ", ")
	}
	return val
}

func cmdVarList(c *cli, args []string) error {
	fs := c.flags("var list")
	var s scopeFlags
	fs.StringVar(&s.scope, "scope", "", "only this scope: private|team|org")
	fs.StringVar(&s.team, "team", "", "only this team `NAME|ID`")
	if _, err := c.parse(fs, args, 0, 0); err != nil {
		return err
	}
	filter := s.scope != "" || s.team != ""
	var apiScope string
	var teamID *string
	if filter {
		if s.scope == "" {
			s.scope = "team"
		}
		var err error
		if apiScope, teamID, err = c.resolveScope(s); err != nil {
			return err
		}
	}
	l, raw, err := c.varList()
	if err != nil {
		return err
	}
	var rows [][]string
	for _, v := range l {
		if filter && !matchesScope(v, apiScope, teamID) {
			continue
		}
		rows = append(rows, []string{v.Name, v.Kind, v.scopeLabel(), varValueCell(v), ago(c.now(), v.UpdatedAt)})
	}
	return c.emit(raw, []string{"name", "kind", "scope", "value", "updated"}, rows)
}

type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(s string) error { *l = append(*l, s); return nil }

func cmdVarSet(c *cli, args []string) error {
	fs := c.flags("var set")
	var s scopeFlags
	addScopeFlags(fs, &s, "scope")
	secret := fs.Bool("secret", false, "store as a secret (value from stdin or --from-file)")
	var hosts stringList
	fs.Var(&hosts, "host", "only inject for requests to this `HOST` (repeatable)")
	var value string
	valueSet := false
	fs.Func("value", "plain value `V` (not for secrets)", func(v string) error { value, valueSet = v, true; return nil })
	fromFile := fs.String("from-file", "", "read the value from file `F` (- = stdin)")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	name := pos[0]
	switch {
	case valueSet && *secret:
		return usagef("secret values are read from stdin or --from-file")
	case valueSet && *fromFile != "":
		return usagef("--value cannot be combined with --from-file")
	}
	apiScope, teamID, err := c.resolveScope(s)
	if err != nil {
		return err
	}
	old, exists, err := c.lookupVar(name, apiScope, teamID)
	if err != nil {
		return err
	}
	// Re-sending a secret as plain would put its value in argv and the API refuses it anyway.
	if exists && old.Kind == "secret" && !*secret {
		return usagef("%s is a secret: use --secret and pass the value on stdin or --from-file", name)
	}
	if !valueSet {
		if value, err = c.secret("Value for "+name+": ", *fromFile); err != nil {
			return err
		}
	}
	kind := "env"
	if *secret {
		kind = "secret"
	}
	label := scopeLabel(apiScope, s.team)
	var raw json.RawMessage
	var path string
	if exists {
		label = old.scopeLabel()
		if path, err = c.orgPath("/env/" + old.ID); err != nil {
			return err
		}
		body := struct {
			Value string    `json:"value"`
			Kind  string    `json:"kind"`
			Hosts *[]string `json:"hosts,omitempty"`
		}{Value: value, Kind: kind}
		if len(hosts) > 0 {
			body.Hosts = (*[]string)(&hosts)
		}
		err = c.call(http.MethodPatch, path, body, nil)
	} else {
		if path, err = c.orgPath("/env"); err != nil {
			return err
		}
		err = c.call(http.MethodPost, path, struct {
			Name   string   `json:"name"`
			Value  string   `json:"value"`
			Kind   string   `json:"kind"`
			Scope  string   `json:"scope"`
			TeamID *string  `json:"teamId,omitempty"`
			Hosts  []string `json:"hosts"`
		}{name, value, kind, apiScope, teamID, append([]string{}, hosts...)}, &raw)
	}
	if err != nil {
		return err
	}
	return c.done(raw, "Set %s (%s).", name, label)
}

func cmdVarMove(c *cli, args []string) error {
	fs := c.flags("var move")
	var s scopeFlags
	addScopeFlags(fs, &s, "scope")
	var to scopeFlags
	fs.StringVar(&to.scope, "to", "", "target scope: private|team|org")
	fs.StringVar(&to.team, "to-team", "", "target team `NAME|ID` (implies --to team)")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	switch {
	case to.scope == "" && to.team == "":
		return usagef("--to is required")
	case to.scope == "":
		to.scope = "team"
	}
	v, err := c.findVar(pos[0], s)
	if err != nil {
		return err
	}
	apiScope, teamID, err := c.resolveScope(to)
	if err != nil {
		return err
	}
	path, err := c.orgPath("/env/" + v.ID + "/move")
	if err != nil {
		return err
	}
	if err := c.call(http.MethodPost, path, struct {
		Scope  string  `json:"scope"`
		TeamID *string `json:"teamId,omitempty"`
	}{apiScope, teamID}, nil); err != nil {
		return err
	}
	return c.done(nil, "Moved %s to %s.", v.Name, scopeLabel(apiScope, to.team))
}

func cmdVarDelete(c *cli, args []string) error {
	fs := c.flags("var delete")
	var s scopeFlags
	addScopeFlags(fs, &s, "scope")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	v, err := c.findVar(pos[0], s)
	if err != nil {
		return err
	}
	if err := c.confirm(fmt.Sprintf("Delete %s (%s)?", v.Name, v.scopeLabel())); err != nil {
		return err
	}
	path, err := c.orgPath("/env/" + v.ID)
	if err != nil {
		return err
	}
	if err := c.call(http.MethodDelete, path, nil, nil); err != nil {
		return err
	}
	return c.done(nil, "Deleted %s (%s).", v.Name, v.scopeLabel())
}
