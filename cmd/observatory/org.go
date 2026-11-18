package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

type memberJSON struct {
	ID       string    `json:"id"`
	Email    string    `json:"email"`
	Name     string    `json:"name"`
	Role     string    `json:"role"`
	JoinedAt time.Time `json:"joinedAt"`
}

type teamJSON struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	MemberCount int       `json:"memberCount"`
	CreatedAt   time.Time `json:"createdAt"`
}

type ruleJSON struct {
	Effect string `json:"effect"`
	Object string `json:"object"`
	Action string `json:"action"`
}

type roleJSON struct {
	ID          string     `json:"id"`
	Key         string     `json:"key"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Builtin     bool       `json:"builtin"`
	Rules       []ruleJSON `json:"rules"`
	Parents     []string   `json:"parents"`
}

func (r roleJSON) ref() ref {
	if r.Builtin {
		return ref{r.Key, r.Name}
	}
	return ref{r.ID, r.Name}
}

type limitsJSON struct {
	VCPU          *int `json:"vcpu"`
	RAMMB         *int `json:"ramMb"`
	DiskGB        *int `json:"diskGb"`
	VMCount       *int `json:"vmCount"`
	SnapshotCount *int `json:"snapshotCount"`
}

func orgCommands() []command {
	return []command{
		{"team list", "", "list teams", cmdTeamList},
		{"team show", "<team>", "show a team and its members", cmdTeamShow},
		{"team create", "<name>", "create a team", cmdTeamCreate},
		{"team rename", "<team> <name>", "rename a team", cmdTeamRename},
		{"team delete", "<team>", "delete a team", cmdTeamDelete},
		{"team add", "<team> <member> [--admin]", "add a member to a team", cmdTeamAdd},
		{"team remove", "<team> <member>", "remove a member from a team", cmdTeamRemove},
		{"member list", "", "list org members", cmdMemberList},
		{"member role", "<member> owner|admin|member", "change a member's org role", cmdMemberRole},
		{"member remove", "<member>", "remove a member from the org", cmdMemberRemove},
		{"invite list", "", "list open invites", cmdInviteList},
		{"invite create", "[--role member]", "create an invite link", cmdInviteCreate},
		{"invite revoke", "<id>", "revoke an invite", cmdInviteRevoke},
		{"invite accept", "[--from-file F|-]", "join an org with an invite (token or URL from stdin or --from-file)", cmdInviteAccept},
		{"role list", "", "list roles", cmdRoleList},
		{"role show", "<role>", "show a role's rules", cmdRoleShow},
		{"role catalog", "", "list the permissions a rule can name", cmdRoleCatalog},
		{"role create", "--file F", "create a custom role from a JSON file", cmdRoleCreate},
		{"role update", "<role> --file F", "replace a custom role from a JSON file", cmdRoleUpdate},
		{"role delete", "<role>", "delete a custom role", cmdRoleDelete},
		{"binding list", "", "list role bindings", cmdBindingList},
		{"binding add", "<subject> <role>", "bind a custom role to user:<x> or team:<x>", cmdBindingAdd},
		{"binding delete", "<id>", "delete a role binding", cmdBindingDelete},
		{"limit show", "", "show resource limits", cmdLimitShow},
		{"limit set", "<team:T|user:M> [flags]", "set resource limits", cmdLimitSet},
		{"session list", "", "list your sessions", cmdSessionList},
		{"session revoke", "<id>", "revoke a session", cmdSessionRevoke},
	}
}

func getList[T any](c *cli, rest string) ([]T, json.RawMessage, error) {
	path, err := c.orgPath(rest)
	if err != nil {
		return nil, nil, err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodGet, path, nil, &raw); err != nil {
		return nil, nil, err
	}
	var l []T
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, nil, err
	}
	return l, raw, nil
}

func findBy[T any](kind, key string, l []T, r func(T) ref) (T, error) {
	refs := make([]ref, len(l))
	for i, it := range l {
		refs[i] = r(it)
	}
	got, err := pick(kind, key, refs)
	if err != nil {
		var zero T
		return zero, err
	}
	return l[slices.IndexFunc(refs, func(x ref) bool { return x.ID == got.ID })], nil
}

func (c *cli) findMember(key string) (memberJSON, error) {
	l, _, err := getList[memberJSON](c, "/members")
	if err != nil {
		return memberJSON{}, err
	}
	for _, m := range l {
		if m.ID == key {
			return m, nil
		}
	}
	for _, m := range l {
		if strings.EqualFold(m.Email, key) {
			return m, nil
		}
	}
	return findBy("member", key, l, func(m memberJSON) ref { return ref{m.ID, m.Name} })
}

func (c *cli) findTeam(key string) (teamJSON, error) {
	l, _, err := getList[teamJSON](c, "/teams")
	if err != nil {
		return teamJSON{}, err
	}
	return findBy("team", key, l, func(t teamJSON) ref { return ref{t.ID, t.Name} })
}

func (c *cli) findRole(key string) (roleJSON, error) {
	l, _, err := getList[roleJSON](c, "/roles")
	if err != nil {
		return roleJSON{}, err
	}
	return findBy("role", key, l, roleJSON.ref)
}

func (c *cli) customRole(key string) (roleJSON, error) {
	r, err := c.findRole(key)
	if err == nil && r.Builtin {
		return r, usagef("role %s is built in", r.Name)
	}
	return r, err
}

func day(t time.Time) string { return t.Format("2006-01-02") }

func (c *cli) simpleCall(method, rest string, body any) error {
	path, err := c.orgPath(rest)
	if err != nil {
		return err
	}
	return c.call(method, path, body, nil)
}

func cmdTeamList(c *cli, args []string) error {
	if _, err := c.parse(c.flags("team list"), args, 0, 0); err != nil {
		return err
	}
	l, raw, err := getList[teamJSON](c, "/teams")
	if err != nil {
		return err
	}
	rows := make([][]string, len(l))
	for i, t := range l {
		rows[i] = []string{t.Name, fmt.Sprint(t.MemberCount), day(t.CreatedAt)}
	}
	return c.emit(raw, []string{"name", "members", "created"}, rows)
}

func cmdTeamShow(c *cli, args []string) error {
	pos, err := c.parse(c.flags("team show"), args, 1, 1)
	if err != nil {
		return err
	}
	t, err := c.findTeam(pos[0])
	if err != nil {
		return err
	}
	path, err := c.orgPath("/teams/" + t.ID)
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodGet, path, nil, &raw); err != nil {
		return err
	}
	var res struct {
		Members []struct {
			Name, Email string
			Admin       bool
		}
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	if c.g.json {
		return c.printJSON(raw)
	}
	if err := c.emitKV(nil, [][2]string{{"name", t.Name}, {"id", t.ID}}); err != nil {
		return err
	}
	fmt.Fprintln(c.stdout)
	rows := make([][]string, len(res.Members))
	for i, m := range res.Members {
		role := "member"
		if m.Admin {
			role = "admin"
		}
		rows[i] = []string{m.Name, m.Email, role}
	}
	return c.emit(nil, []string{"name", "email", "team role"}, rows)
}

func cmdTeamCreate(c *cli, args []string) error {
	pos, err := c.parse(c.flags("team create"), args, 1, 1)
	if err != nil {
		return err
	}
	path, err := c.orgPath("/teams")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodPost, path, map[string]string{"name": pos[0]}, &raw); err != nil {
		return err
	}
	var t teamJSON
	if err := json.Unmarshal(raw, &t); err != nil {
		return err
	}
	return c.done(raw, "Team %s created.", t.Name)
}

func cmdTeamRename(c *cli, args []string) error {
	pos, err := c.parse(c.flags("team rename"), args, 2, 2)
	if err != nil {
		return err
	}
	t, err := c.findTeam(pos[0])
	if err != nil {
		return err
	}
	if err := c.simpleCall(http.MethodPatch, "/teams/"+t.ID, map[string]string{"name": pos[1]}); err != nil {
		return err
	}
	return c.done(nil, "Team %s renamed to %s.", t.Name, pos[1])
}

func cmdTeamDelete(c *cli, args []string) error {
	pos, err := c.parse(c.flags("team delete"), args, 1, 1)
	if err != nil {
		return err
	}
	t, err := c.findTeam(pos[0])
	if err != nil {
		return err
	}
	if err := c.confirm(fmt.Sprintf("Delete team %s?", t.Name)); err != nil {
		return err
	}
	if err := c.simpleCall(http.MethodDelete, "/teams/"+t.ID, nil); err != nil {
		return err
	}
	return c.done(nil, "Team %s deleted.", t.Name)
}

func teamMemberArgs(c *cli, fs *flag.FlagSet, args []string) (teamJSON, memberJSON, error) {
	pos, err := c.parse(fs, args, 2, 2)
	if err != nil {
		return teamJSON{}, memberJSON{}, err
	}
	t, err := c.findTeam(pos[0])
	if err != nil {
		return t, memberJSON{}, err
	}
	m, err := c.findMember(pos[1])
	return t, m, err
}

func cmdTeamAdd(c *cli, args []string) error {
	fs := c.flags("team add")
	admin := fs.Bool("admin", false, "make the member a team admin")
	t, m, err := teamMemberArgs(c, fs, args)
	if err != nil {
		return err
	}
	if err := c.simpleCall(http.MethodPut, "/teams/"+t.ID+"/members/"+m.ID, map[string]bool{"admin": *admin}); err != nil {
		return err
	}
	return c.done(nil, "%s added to team %s.", m.Name, t.Name)
}

func cmdTeamRemove(c *cli, args []string) error {
	t, m, err := teamMemberArgs(c, c.flags("team remove"), args)
	if err != nil {
		return err
	}
	if err := c.confirm(fmt.Sprintf("Remove %s from team %s?", m.Name, t.Name)); err != nil {
		return err
	}
	if err := c.simpleCall(http.MethodDelete, "/teams/"+t.ID+"/members/"+m.ID, nil); err != nil {
		return err
	}
	return c.done(nil, "%s removed from team %s.", m.Name, t.Name)
}

func cmdMemberList(c *cli, args []string) error {
	if _, err := c.parse(c.flags("member list"), args, 0, 0); err != nil {
		return err
	}
	l, raw, err := getList[memberJSON](c, "/members")
	if err != nil {
		return err
	}
	rows := make([][]string, len(l))
	for i, m := range l {
		rows[i] = []string{m.Name, m.Email, m.Role, day(m.JoinedAt)}
	}
	return c.emit(raw, []string{"name", "email", "role", "joined"}, rows)
}

func validOrgRole(role string) error {
	if !slices.Contains([]string{"owner", "admin", "member"}, role) {
		return usagef("unknown role %q: use owner, admin or member", role)
	}
	return nil
}

func cmdMemberRole(c *cli, args []string) error {
	pos, err := c.parse(c.flags("member role"), args, 2, 2)
	if err != nil {
		return err
	}
	if err := validOrgRole(pos[1]); err != nil {
		return err
	}
	m, err := c.findMember(pos[0])
	if err != nil {
		return err
	}
	if err := c.simpleCall(http.MethodPatch, "/members/"+m.ID, map[string]string{"role": pos[1]}); err != nil {
		return err
	}
	return c.done(nil, "%s is now %s.", m.Name, pos[1])
}

func cmdMemberRemove(c *cli, args []string) error {
	pos, err := c.parse(c.flags("member remove"), args, 1, 1)
	if err != nil {
		return err
	}
	m, err := c.findMember(pos[0])
	if err != nil {
		return err
	}
	if err := c.confirm(fmt.Sprintf("Remove %s from the org?", m.Name)); err != nil {
		return err
	}
	if err := c.simpleCall(http.MethodDelete, "/members/"+m.ID, nil); err != nil {
		return err
	}
	return c.done(nil, "%s removed from the org.", m.Name)
}

func cmdInviteList(c *cli, args []string) error {
	if _, err := c.parse(c.flags("invite list"), args, 0, 0); err != nil {
		return err
	}
	l, raw, err := getList[struct {
		ID, Role             string
		CreatedAt, ExpiresAt time.Time
	}](c, "/invites")
	if err != nil {
		return err
	}
	rows := make([][]string, len(l))
	for i, v := range l {
		rows[i] = []string{v.ID, v.Role, day(v.CreatedAt), day(v.ExpiresAt)}
	}
	return c.emit(raw, []string{"id", "role", "created", "expires"}, rows)
}

func cmdInviteCreate(c *cli, args []string) error {
	fs := c.flags("invite create")
	role := fs.String("role", "member", "org role: owner|admin|member")
	if _, err := c.parse(fs, args, 0, 0); err != nil {
		return err
	}
	if err := validOrgRole(*role); err != nil {
		return err
	}
	path, err := c.orgPath("/invites")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodPost, path, map[string]string{"role": *role}, &raw); err != nil {
		return err
	}
	var res struct{ URL string }
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	return c.done(raw, "%s", res.URL)
}

func cmdInviteRevoke(c *cli, args []string) error {
	pos, err := c.parse(c.flags("invite revoke"), args, 1, 1)
	if err != nil {
		return err
	}
	if err := c.confirm(fmt.Sprintf("Revoke invite %s?", pos[0])); err != nil {
		return err
	}
	if err := c.simpleCall(http.MethodDelete, "/invites/"+pos[0], nil); err != nil {
		return err
	}
	return c.done(nil, "Invite %s revoked.", pos[0])
}

func cmdInviteAccept(c *cli, args []string) error {
	fs := c.flags("invite accept")
	file := fs.String("from-file", "", "read the token or URL from `F` (- for stdin)")
	pos, err := c.parse(fs, args, 0, -1)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("the invite token is not taken as an argument: pipe it on stdin or use --from-file")
	}
	token, err := c.secret("Invite token or URL: ", *file)
	if err != nil {
		return err
	}
	token = strings.TrimRight(token, "/")
	token = token[strings.LastIndex(token, "/")+1:]
	var raw json.RawMessage
	if err := c.call(http.MethodPost, "/api/invites/accept", map[string]string{"token": token}, &raw); err != nil {
		return err
	}
	var res struct {
		OrgID string `json:"orgId"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	name := res.OrgID
	if me, err := c.me(nil); err == nil {
		if i := slices.IndexFunc(me.Orgs, func(o orgJSON) bool { return o.ID == res.OrgID }); i >= 0 {
			name = me.Orgs[i].Name
		}
	}
	return c.done(raw, "Joined %s.", name)
}

func cmdRoleList(c *cli, args []string) error {
	if _, err := c.parse(c.flags("role list"), args, 0, 0); err != nil {
		return err
	}
	l, raw, err := getList[roleJSON](c, "/roles")
	if err != nil {
		return err
	}
	rows := make([][]string, len(l))
	for i, r := range l {
		kind := "custom"
		if r.Builtin {
			kind = "built-in"
		}
		rows[i] = []string{r.Name, r.ref().ID, kind, fmt.Sprint(len(r.Rules))}
	}
	return c.emit(raw, []string{"name", "id", "kind", "rules"}, rows)
}

func cmdRoleShow(c *cli, args []string) error {
	pos, err := c.parse(c.flags("role show"), args, 1, 1)
	if err != nil {
		return err
	}
	l, _, err := getList[roleJSON](c, "/roles")
	if err != nil {
		return err
	}
	r, err := findBy("role", pos[0], l, roleJSON.ref)
	if err != nil {
		return err
	}
	if c.g.json {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		return c.printJSON(b)
	}
	pairs := [][2]string{{"name", r.Name}, {"id", r.ref().ID}}
	if r.Description != "" {
		pairs = append(pairs, [2]string{"description", r.Description})
	}
	if len(r.Parents) > 0 {
		pairs = append(pairs, [2]string{"parents", strings.Join(r.Parents, ", ")})
	}
	if err := c.emitKV(nil, pairs); err != nil {
		return err
	}
	fmt.Fprintln(c.stdout)
	rows := make([][]string, len(r.Rules))
	for i, x := range r.Rules {
		rows[i] = []string{x.Effect, x.Object, x.Action}
	}
	return c.emit(nil, []string{"effect", "object", "action"}, rows)
}

func cmdRoleCatalog(c *cli, args []string) error {
	if _, err := c.parse(c.flags("role catalog"), args, 0, 0); err != nil {
		return err
	}
	l, raw, err := getList[struct{ Action, Object string }](c, "/catalog")
	if err != nil {
		return err
	}
	rows := make([][]string, len(l))
	for i, p := range l {
		rows[i] = []string{p.Action, p.Object}
	}
	return c.emit(raw, []string{"action", "object"}, rows)
}

func (c *cli) roleFile(file string) (json.RawMessage, error) {
	if file == "" {
		return nil, usagef("--file is required")
	}
	var b []byte
	var err error
	if file == "-" {
		b, err = io.ReadAll(c.stdin)
	} else {
		b, err = os.ReadFile(file)
	}
	if err != nil {
		return nil, usagef("cannot read %s", file)
	}
	if !json.Valid(b) {
		return nil, usagef("%s is not valid JSON", file)
	}
	return b, nil
}

func cmdRoleCreate(c *cli, args []string) error {
	fs := c.flags("role create")
	file := fs.String("file", "", "role JSON `file` (- for stdin)")
	if _, err := c.parse(fs, args, 0, 0); err != nil {
		return err
	}
	body, err := c.roleFile(*file)
	if err != nil {
		return err
	}
	path, err := c.orgPath("/roles")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodPost, path, body, &raw); err != nil {
		return err
	}
	var r roleJSON
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	return c.done(raw, "Role %s created.", r.Name)
}

func cmdRoleUpdate(c *cli, args []string) error {
	fs := c.flags("role update")
	file := fs.String("file", "", "role JSON `file` (- for stdin)")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	body, err := c.roleFile(*file)
	if err != nil {
		return err
	}
	r, err := c.customRole(pos[0])
	if err != nil {
		return err
	}
	if err := c.simpleCall(http.MethodPut, "/roles/"+r.ID, body); err != nil {
		return err
	}
	return c.done(nil, "Role %s updated.", r.Name)
}

func cmdRoleDelete(c *cli, args []string) error {
	pos, err := c.parse(c.flags("role delete"), args, 1, 1)
	if err != nil {
		return err
	}
	r, err := c.customRole(pos[0])
	if err != nil {
		return err
	}
	if err := c.confirm(fmt.Sprintf("Delete role %s?", r.Name)); err != nil {
		return err
	}
	if err := c.simpleCall(http.MethodDelete, "/roles/"+r.ID, nil); err != nil {
		return err
	}
	return c.done(nil, "Role %s deleted.", r.Name)
}

func cmdBindingList(c *cli, args []string) error {
	if _, err := c.parse(c.flags("binding list"), args, 0, 0); err != nil {
		return err
	}
	l, raw, err := getList[struct {
		ID, Subject, RoleID string
		CreatedAt           time.Time
	}](c, "/bindings")
	if err != nil {
		return err
	}
	roles, _, err := getList[roleJSON](c, "/roles")
	if err != nil {
		return err
	}
	rows := make([][]string, len(l))
	for i, b := range l {
		role := b.RoleID
		if j := slices.IndexFunc(roles, func(r roleJSON) bool { return r.ID == b.RoleID }); j >= 0 {
			role = roles[j].Name
		}
		rows[i] = []string{b.ID, b.Subject, role, day(b.CreatedAt)}
	}
	return c.emit(raw, []string{"id", "subject", "role", "created"}, rows)
}

func (c *cli) resolveSubject(s string) (string, error) {
	kind, key, _ := strings.Cut(s, ":")
	switch kind {
	case "user":
		m, err := c.findMember(key)
		return "user:" + m.ID, err
	case "team":
		t, err := c.findTeam(key)
		return "team:" + t.ID, err
	}
	return s, nil
}

func cmdBindingAdd(c *cli, args []string) error {
	pos, err := c.parse(c.flags("binding add"), args, 2, 2)
	if err != nil {
		return err
	}
	subject, err := c.resolveSubject(pos[0])
	if err != nil {
		return err
	}
	r, err := c.customRole(pos[1])
	if err != nil {
		return err
	}
	path, err := c.orgPath("/bindings")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodPost, path, map[string]string{"subject": subject, "roleId": r.ID}, &raw); err != nil {
		return err
	}
	return c.done(raw, "Bound %s to role %s.", pos[0], r.Name)
}

func cmdBindingDelete(c *cli, args []string) error {
	pos, err := c.parse(c.flags("binding delete"), args, 1, 1)
	if err != nil {
		return err
	}
	if err := c.confirm(fmt.Sprintf("Delete binding %s?", pos[0])); err != nil {
		return err
	}
	if err := c.simpleCall(http.MethodDelete, "/bindings/"+pos[0], nil); err != nil {
		return err
	}
	return c.done(nil, "Binding %s deleted.", pos[0])
}

func limitCell(v *int, f func(int) string) string {
	if v == nil {
		return "none"
	}
	return f(*v)
}

func limitRow(scope string, l limitsJSON) []string {
	num := func(n int) string { return strconv.Itoa(n) }
	return []string{scope, limitCell(l.VCPU, num), limitCell(l.RAMMB, gb), limitCell(l.DiskGB, func(n int) string { return gb(n * 1024) }),
		limitCell(l.VMCount, num), limitCell(l.SnapshotCount, num)}
}

type orgLimits struct {
	Org     limitsJSON            `json:"org"`
	Teams   map[string]limitsJSON `json:"teams"`
	Members map[string]limitsJSON `json:"members"`
}

func (c *cli) orgLimits() (orgLimits, json.RawMessage, error) {
	var ol orgLimits
	path, err := c.orgPath("/limits")
	if err != nil {
		return ol, nil, err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodGet, path, nil, &raw); err != nil {
		return ol, nil, err
	}
	if err := json.Unmarshal(raw, &ol); err != nil {
		return ol, nil, err
	}
	return ol, raw, nil
}

func sortedKeys(m map[string]limitsJSON) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func cmdLimitShow(c *cli, args []string) error {
	if _, err := c.parse(c.flags("limit show"), args, 0, 0); err != nil {
		return err
	}
	ol, raw, err := c.orgLimits()
	if err != nil {
		return err
	}
	rows := [][]string{limitRow("org", ol.Org)}
	if !c.g.json {
		names := map[string]string{}
		if len(ol.Teams) > 0 {
			ts, _, err := getList[teamJSON](c, "/teams")
			if err != nil {
				return err
			}
			for _, t := range ts {
				names["team:"+t.ID] = t.Name
			}
		}
		if len(ol.Members) > 0 {
			ms, _, err := getList[memberJSON](c, "/members")
			if err != nil {
				return err
			}
			for _, m := range ms {
				names["user:"+m.ID] = m.Name
			}
		}
		for _, s := range []struct {
			prefix string
			set    map[string]limitsJSON
		}{{"team:", ol.Teams}, {"user:", ol.Members}} {
			for _, id := range sortedKeys(s.set) {
				label := s.prefix + id
				if n := names[label]; n != "" {
					label += " (" + n + ")"
				}
				rows = append(rows, limitRow(label, s.set[id]))
			}
		}
	}
	return c.emit(raw, []string{"scope", "vcpu", "ram", "disk", "vms", "snapshots"}, rows)
}

func parseLimit(name, v string) (*int, error) {
	if v == "none" {
		return nil, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return nil, usagef("--%s must be a non-negative number or none", name)
	}
	return &n, nil
}

func cmdLimitSet(c *cli, args []string) error {
	fs := c.flags("limit set")
	vals := map[string]*string{}
	for _, f := range [][2]string{{"vcpu", "vCPUs"}, {"ram", "RAM in MB"}, {"disk", "disk in GB"}, {"vms", "VM count"}, {"snapshots", "snapshot count"}} {
		vals[f[0]] = fs.String(f[0], "", f[1]+" or none")
	}
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	set := map[string]string{}
	fs.Visit(func(f *flag.Flag) {
		if _, ok := vals[f.Name]; ok {
			set[f.Name] = f.Value.String()
		}
	})
	if len(set) == 0 {
		return usagef("pass at least one of --vcpu, --ram, --disk, --vms, --snapshots")
	}
	parsed := map[string]*int{}
	for name, v := range set {
		if parsed[name], err = parseLimit(name, v); err != nil {
			return err
		}
	}
	kind, key, _ := strings.Cut(pos[0], ":")
	var scope string
	switch {
	case pos[0] == "org":
		return usagef("org limits are set by a platform admin in the web UI")
	case kind == "team" && key != "":
		t, err := c.findTeam(key)
		if err != nil {
			return err
		}
		scope = "team:" + t.ID
	case kind == "user" && key != "":
		m, err := c.findMember(key)
		if err != nil {
			return err
		}
		scope = "user:" + m.ID
	default:
		return usagef("scope must be org, team:<team> or user:<member>")
	}
	ol, _, err := c.orgLimits()
	if err != nil {
		return err
	}
	cur := ol.Teams[strings.TrimPrefix(scope, "team:")]
	if kind == "user" {
		cur = ol.Members[strings.TrimPrefix(scope, "user:")]
	}
	for name, p := range map[string]**int{"vcpu": &cur.VCPU, "ram": &cur.RAMMB, "disk": &cur.DiskGB, "vms": &cur.VMCount, "snapshots": &cur.SnapshotCount} {
		if _, ok := parsed[name]; ok {
			*p = parsed[name]
		}
	}
	if err := c.simpleCall(http.MethodPut, "/limits/"+scope, cur); err != nil {
		return err
	}
	return c.done(nil, "Limits for %s set.", pos[0])
}

type sessionJSON struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	UserAgent  string    `json:"userAgent"`
	IP         string    `json:"ip"`
	LastUsedAt time.Time `json:"lastUsedAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	Current    bool      `json:"current"`
}

func (c *cli) sessions() ([]sessionJSON, json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.call(http.MethodGet, "/api/me/sessions", nil, &raw); err != nil {
		return nil, nil, err
	}
	var l []sessionJSON
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, nil, err
	}
	return l, raw, nil
}

func cmdSessionList(c *cli, args []string) error {
	if _, err := c.parse(c.flags("session list"), args, 0, 0); err != nil {
		return err
	}
	l, raw, err := c.sessions()
	if err != nil {
		return err
	}
	rows := make([][]string, len(l))
	for i, s := range l {
		mark := ""
		if s.Current {
			mark = "*"
		}
		rows[i] = []string{mark, s.ID, s.Kind, s.IP, s.UserAgent, ago(c.now(), s.LastUsedAt), s.ExpiresAt.In(c.now().Location()).Format("2006-01-02 15:04")}
	}
	return c.emit(raw, []string{"", "id", "kind", "ip", "user agent", "last used", "expires"}, rows)
}

func cmdSessionRevoke(c *cli, args []string) error {
	pos, err := c.parse(c.flags("session revoke"), args, 1, 1)
	if err != nil {
		return err
	}
	l, _, err := c.sessions()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(l, func(s sessionJSON) bool { return s.ID == pos[0] })
	if i < 0 {
		return notFoundf("session %q not found", pos[0])
	}
	if err := c.confirm(fmt.Sprintf("Revoke session %s?", pos[0])); err != nil {
		return err
	}
	if err := c.call(http.MethodDelete, "/api/me/sessions/"+pos[0], nil, nil); err != nil {
		var ae *apiError
		if !(l[i].Current && errors.As(err, &ae) && ae.Status == http.StatusUnauthorized) {
			return err
		}
	}
	if l[i].Current {
		c.cfg.Token, c.cfg.ExpiresAt = "", time.Time{}
		if err := saveConfig(c.cfgDir, c.cfg); err != nil {
			return err
		}
	}
	return c.done(nil, "Session %s revoked.", pos[0])
}
