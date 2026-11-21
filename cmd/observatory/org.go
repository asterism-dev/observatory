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

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
)

func roleRef(r *iamv1.Role) ref {
	if r.GetBuiltin() {
		return ref{r.GetKey(), r.GetName()}
	}
	return ref{r.GetId(), r.GetName()}
}

type limitsJSON struct {
	VCPU           *int `json:"vcpu"`
	RAMMB          *int `json:"ramMb"`
	DiskGB         *int `json:"diskGb"`
	VMCount        *int `json:"vmCount"`
	SnapshotCount  *int `json:"snapshotCount"`
	ImageStorageGB *int `json:"imageStorageGb"`
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

func (c *cli) simpleCall(method, rest string, body any) error {
	path, err := c.orgPath(rest)
	if err != nil {
		return err
	}
	return c.call(method, path, body, nil)
}

// orgRPC calls an org-scoped IAMService method on the current org, which every such request names in its org field.
func (c *cli) orgRPC(method string, req, resp proto.Message) error {
	o, err := c.currentOrg()
	if err != nil {
		return err
	}
	m := req.ProtoReflect()
	f := m.Descriptor().Fields().ByName("org")
	if f == nil {
		return fmt.Errorf("%s: request has no org field", method)
	}
	m.Set(f, protoreflect.ValueOfString(o.GetId()))
	return c.rpc(method, req, resp)
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

func (c *cli) members() (*iamv1.ListMembersResponse, error) {
	res := &iamv1.ListMembersResponse{}
	return res, c.orgRPC("ListMembers", &iamv1.ListMembersRequest{}, res)
}

func (c *cli) teams() (*iamv1.IAMServiceListTeamsResponse, error) {
	res := &iamv1.IAMServiceListTeamsResponse{}
	return res, c.orgRPC("ListTeams", &iamv1.IAMServiceListTeamsRequest{}, res)
}

func (c *cli) roles() ([]*iamv1.Role, error) {
	res := &iamv1.ListRolesResponse{}
	err := c.orgRPC("ListRoles", &iamv1.ListRolesRequest{}, res)
	return res.GetRoles(), err
}

func (c *cli) findMember(key string) (*iamv1.Member, error) {
	res, err := c.members()
	if err != nil {
		return nil, err
	}
	l := res.GetMembers()
	for _, m := range l {
		if m.GetId() == key {
			return m, nil
		}
	}
	for _, m := range l {
		if strings.EqualFold(m.GetEmail(), key) {
			return m, nil
		}
	}
	return findBy("member", key, l, func(m *iamv1.Member) ref { return ref{m.GetId(), m.GetName()} })
}

func (c *cli) findTeam(key string) (*iamv1.TeamInfo, error) {
	res, err := c.teams()
	if err != nil {
		return nil, err
	}
	return findBy("team", key, res.GetTeams(), func(t *iamv1.TeamInfo) ref { return ref{t.GetId(), t.GetName()} })
}

func (c *cli) findRole(key string) (*iamv1.Role, error) {
	l, err := c.roles()
	if err != nil {
		return nil, err
	}
	return findBy("role", key, l, roleRef)
}

func (c *cli) customRole(key string) (*iamv1.Role, error) {
	r, err := c.findRole(key)
	if err == nil && r.GetBuiltin() {
		return r, usagef("role %s is built in", r.GetName())
	}
	return r, err
}

func day(t *timestamppb.Timestamp) string { return t.AsTime().Format("2006-01-02") }

func cmdTeamList(c *cli, args []string) error {
	if _, err := c.parse(c.flags("team list"), args, 0, 0); err != nil {
		return err
	}
	res, err := c.teams()
	if err != nil {
		return err
	}
	rows := make([][]string, len(res.GetTeams()))
	for i, t := range res.GetTeams() {
		rows[i] = []string{t.GetName(), fmt.Sprint(t.GetMemberCount()), day(t.GetCreatedAt())}
	}
	return c.emit(pj(res), []string{"name", "members", "created"}, rows)
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
	res := &iamv1.GetTeamResponse{}
	if err := c.orgRPC("GetTeam", &iamv1.GetTeamRequest{Team: t.GetId()}, res); err != nil {
		return err
	}
	if c.g.json {
		return c.printJSON(pj(res))
	}
	if err := c.emitKV(nil, [][2]string{{"name", t.GetName()}, {"id", t.GetId()}}); err != nil {
		return err
	}
	fmt.Fprintln(c.stdout)
	rows := make([][]string, len(res.GetMembers()))
	for i, m := range res.GetMembers() {
		role := "member"
		if m.GetAdmin() {
			role = "admin"
		}
		rows[i] = []string{m.GetName(), m.GetEmail(), role}
	}
	return c.emit(nil, []string{"name", "email", "team role"}, rows)
}

func cmdTeamCreate(c *cli, args []string) error {
	pos, err := c.parse(c.flags("team create"), args, 1, 1)
	if err != nil {
		return err
	}
	res := &iamv1.CreateTeamResponse{}
	if err := c.orgRPC("CreateTeam", &iamv1.CreateTeamRequest{Name: pos[0]}, res); err != nil {
		return err
	}
	return c.done(pj(res), "Team %s created.", res.GetTeam().GetName())
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
	if err := c.orgRPC("UpdateTeam", &iamv1.UpdateTeamRequest{Team: t.GetId(), Name: pos[1]}, &iamv1.UpdateTeamResponse{}); err != nil {
		return err
	}
	return c.done(nil, "Team %s renamed to %s.", t.GetName(), pos[1])
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
	if err := c.confirm(fmt.Sprintf("Delete team %s?", t.GetName())); err != nil {
		return err
	}
	if err := c.orgRPC("DeleteTeam", &iamv1.DeleteTeamRequest{Team: t.GetId()}, &iamv1.DeleteTeamResponse{}); err != nil {
		return err
	}
	return c.done(nil, "Team %s deleted.", t.GetName())
}

func teamMemberArgs(c *cli, fs *flag.FlagSet, args []string) (*iamv1.TeamInfo, *iamv1.Member, error) {
	pos, err := c.parse(fs, args, 2, 2)
	if err != nil {
		return nil, nil, err
	}
	t, err := c.findTeam(pos[0])
	if err != nil {
		return t, nil, err
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
	if err := c.orgRPC("SetTeamMember", &iamv1.SetTeamMemberRequest{Team: t.GetId(), User: m.GetId(), Admin: admin}, &iamv1.SetTeamMemberResponse{}); err != nil {
		return err
	}
	return c.done(nil, "%s added to team %s.", m.GetName(), t.GetName())
}

func cmdTeamRemove(c *cli, args []string) error {
	t, m, err := teamMemberArgs(c, c.flags("team remove"), args)
	if err != nil {
		return err
	}
	if err := c.confirm(fmt.Sprintf("Remove %s from team %s?", m.GetName(), t.GetName())); err != nil {
		return err
	}
	if m.GetId() == c.userID {
		err = c.orgRPC("LeaveTeam", &iamv1.LeaveTeamRequest{Team: t.GetId()}, &iamv1.LeaveTeamResponse{})
	} else {
		err = c.orgRPC("RemoveTeamMember", &iamv1.RemoveTeamMemberRequest{Team: t.GetId(), User: m.GetId()}, &iamv1.RemoveTeamMemberResponse{})
	}
	if err != nil {
		return err
	}
	return c.done(nil, "%s removed from team %s.", m.GetName(), t.GetName())
}

func cmdMemberList(c *cli, args []string) error {
	if _, err := c.parse(c.flags("member list"), args, 0, 0); err != nil {
		return err
	}
	res, err := c.members()
	if err != nil {
		return err
	}
	rows := make([][]string, len(res.GetMembers()))
	for i, m := range res.GetMembers() {
		rows[i] = []string{m.GetName(), m.GetEmail(), m.GetRole(), day(m.GetJoinedAt())}
	}
	return c.emit(pj(res), []string{"name", "email", "role", "joined"}, rows)
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
	if err := c.orgRPC("UpdateMember", &iamv1.UpdateMemberRequest{User: m.GetId(), Role: pos[1]}, &iamv1.UpdateMemberResponse{}); err != nil {
		return err
	}
	return c.done(nil, "%s is now %s.", m.GetName(), pos[1])
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
	if err := c.confirm(fmt.Sprintf("Remove %s from the org?", m.GetName())); err != nil {
		return err
	}
	if m.GetId() == c.userID {
		err = c.rpc("LeaveOrg", &iamv1.LeaveOrgRequest{OrgId: c.org.GetId()}, &iamv1.LeaveOrgResponse{})
	} else {
		err = c.orgRPC("DeleteMember", &iamv1.DeleteMemberRequest{User: m.GetId()}, &iamv1.DeleteMemberResponse{})
	}
	if err != nil {
		return err
	}
	return c.done(nil, "%s removed from the org.", m.GetName())
}

func cmdInviteList(c *cli, args []string) error {
	if _, err := c.parse(c.flags("invite list"), args, 0, 0); err != nil {
		return err
	}
	res := &iamv1.ListInvitesResponse{}
	if err := c.orgRPC("ListInvites", &iamv1.ListInvitesRequest{}, res); err != nil {
		return err
	}
	rows := make([][]string, len(res.GetInvites()))
	for i, v := range res.GetInvites() {
		rows[i] = []string{v.GetId(), v.GetRole(), day(v.GetCreatedAt()), day(v.GetExpiresAt())}
	}
	return c.emit(pj(res), []string{"id", "role", "created", "expires"}, rows)
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
	res := &iamv1.CreateInviteResponse{}
	if err := c.orgRPC("CreateInvite", &iamv1.CreateInviteRequest{Role: *role}, res); err != nil {
		return err
	}
	return c.done(pj(res), "%s", res.GetUrl())
}

func cmdInviteRevoke(c *cli, args []string) error {
	pos, err := c.parse(c.flags("invite revoke"), args, 1, 1)
	if err != nil {
		return err
	}
	if err := c.confirm(fmt.Sprintf("Revoke invite %s?", pos[0])); err != nil {
		return err
	}
	if err := c.orgRPC("DeleteInvite", &iamv1.DeleteInviteRequest{Invite: pos[0]}, &iamv1.DeleteInviteResponse{}); err != nil {
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
	res := &iamv1.AcceptInviteResponse{}
	if err := c.rpc("AcceptInvite", &iamv1.AcceptInviteRequest{Token: token}, res); err != nil {
		return err
	}
	name := res.GetOrgId()
	if me, err := c.me(); err == nil {
		if o, err := findOrg(me.GetOrgs(), res.GetOrgId()); err == nil {
			name = o.GetName()
		}
	}
	return c.done(pj(res), "Joined %s.", name)
}

func cmdRoleList(c *cli, args []string) error {
	if _, err := c.parse(c.flags("role list"), args, 0, 0); err != nil {
		return err
	}
	res := &iamv1.ListRolesResponse{}
	if err := c.orgRPC("ListRoles", &iamv1.ListRolesRequest{}, res); err != nil {
		return err
	}
	rows := make([][]string, len(res.GetRoles()))
	for i, r := range res.GetRoles() {
		kind := "custom"
		if r.GetBuiltin() {
			kind = "built-in"
		}
		rows[i] = []string{r.GetName(), roleRef(r).ID, kind, fmt.Sprint(len(r.GetRules()))}
	}
	return c.emit(pj(res), []string{"name", "id", "kind", "rules"}, rows)
}

func cmdRoleShow(c *cli, args []string) error {
	pos, err := c.parse(c.flags("role show"), args, 1, 1)
	if err != nil {
		return err
	}
	r, err := c.findRole(pos[0])
	if err != nil {
		return err
	}
	if c.g.json {
		return c.printJSON(pj(r))
	}
	pairs := [][2]string{{"name", r.GetName()}, {"id", roleRef(r).ID}}
	if r.GetDescription() != "" {
		pairs = append(pairs, [2]string{"description", r.GetDescription()})
	}
	if len(r.GetParents()) > 0 {
		pairs = append(pairs, [2]string{"parents", strings.Join(r.GetParents(), ", ")})
	}
	if err := c.emitKV(nil, pairs); err != nil {
		return err
	}
	fmt.Fprintln(c.stdout)
	rows := make([][]string, len(r.GetRules()))
	for i, x := range r.GetRules() {
		rows[i] = []string{x.GetEffect(), x.GetObject(), x.GetAction()}
	}
	return c.emit(nil, []string{"effect", "object", "action"}, rows)
}

func cmdRoleCatalog(c *cli, args []string) error {
	if _, err := c.parse(c.flags("role catalog"), args, 0, 0); err != nil {
		return err
	}
	res := &iamv1.ListCatalogResponse{}
	if err := c.orgRPC("ListCatalog", &iamv1.ListCatalogRequest{}, res); err != nil {
		return err
	}
	rows := make([][]string, len(res.GetEntries()))
	for i, p := range res.GetEntries() {
		rows[i] = []string{p.GetAction(), p.GetObject()}
	}
	return c.emit(pj(res), []string{"action", "object"}, rows)
}

// roleFile reads a role (name, description, rules, parents) into req; other fields, such as those of role show --json, are ignored.
func (c *cli) roleFile(file string, req proto.Message) error {
	if file == "" {
		return usagef("--file is required")
	}
	var b []byte
	var err error
	if file == "-" {
		b, err = io.ReadAll(c.stdin)
	} else {
		b, err = os.ReadFile(file)
	}
	if err != nil {
		return usagef("cannot read %s", file)
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, req); err != nil {
		return usagef("%s is not a valid role: %v", file, err)
	}
	return nil
}

func cmdRoleCreate(c *cli, args []string) error {
	fs := c.flags("role create")
	file := fs.String("file", "", "role JSON `file` (- for stdin)")
	if _, err := c.parse(fs, args, 0, 0); err != nil {
		return err
	}
	req := &iamv1.CreateRoleRequest{}
	if err := c.roleFile(*file, req); err != nil {
		return err
	}
	res := &iamv1.CreateRoleResponse{}
	if err := c.orgRPC("CreateRole", req, res); err != nil {
		return err
	}
	return c.done(pj(res), "Role %s created.", res.GetRole().GetName())
}

func cmdRoleUpdate(c *cli, args []string) error {
	fs := c.flags("role update")
	file := fs.String("file", "", "role JSON `file` (- for stdin)")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	req := &iamv1.UpdateRoleRequest{}
	if err := c.roleFile(*file, req); err != nil {
		return err
	}
	r, err := c.customRole(pos[0])
	if err != nil {
		return err
	}
	req.Role = r.GetId()
	if err := c.orgRPC("UpdateRole", req, &iamv1.UpdateRoleResponse{}); err != nil {
		return err
	}
	return c.done(nil, "Role %s updated.", r.GetName())
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
	if err := c.confirm(fmt.Sprintf("Delete role %s?", r.GetName())); err != nil {
		return err
	}
	if err := c.orgRPC("DeleteRole", &iamv1.DeleteRoleRequest{Role: r.GetId()}, &iamv1.DeleteRoleResponse{}); err != nil {
		return err
	}
	return c.done(nil, "Role %s deleted.", r.GetName())
}

func cmdBindingList(c *cli, args []string) error {
	if _, err := c.parse(c.flags("binding list"), args, 0, 0); err != nil {
		return err
	}
	res := &iamv1.ListRoleBindingsResponse{}
	if err := c.orgRPC("ListRoleBindings", &iamv1.ListRoleBindingsRequest{}, res); err != nil {
		return err
	}
	roles, err := c.roles()
	if err != nil {
		return err
	}
	rows := make([][]string, len(res.GetBindings()))
	for i, b := range res.GetBindings() {
		role := b.GetRoleId()
		if j := slices.IndexFunc(roles, func(r *iamv1.Role) bool { return r.GetId() == b.GetRoleId() }); j >= 0 {
			role = roles[j].GetName()
		}
		rows[i] = []string{b.GetId(), b.GetSubject(), role, day(b.GetCreatedAt())}
	}
	return c.emit(pj(res), []string{"id", "subject", "role", "created"}, rows)
}

func (c *cli) resolveSubject(s string) (string, error) {
	kind, key, _ := strings.Cut(s, ":")
	switch kind {
	case "user":
		m, err := c.findMember(key)
		return "user:" + m.GetId(), err
	case "team":
		t, err := c.findTeam(key)
		return "team:" + t.GetId(), err
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
	res := &iamv1.CreateRoleBindingResponse{}
	if err := c.orgRPC("CreateRoleBinding", &iamv1.CreateRoleBindingRequest{Subject: subject, RoleId: r.GetId()}, res); err != nil {
		return err
	}
	return c.done(pj(res), "Bound %s to role %s.", pos[0], r.GetName())
}

func cmdBindingDelete(c *cli, args []string) error {
	pos, err := c.parse(c.flags("binding delete"), args, 1, 1)
	if err != nil {
		return err
	}
	if err := c.confirm(fmt.Sprintf("Delete binding %s?", pos[0])); err != nil {
		return err
	}
	if err := c.orgRPC("DeleteRoleBinding", &iamv1.DeleteRoleBindingRequest{Binding: pos[0]}, &iamv1.DeleteRoleBindingResponse{}); err != nil {
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
		limitCell(l.VMCount, num), limitCell(l.SnapshotCount, num), limitCell(l.ImageStorageGB, func(n int) string { return gb(n * 1024) })}
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
			res, err := c.teams()
			if err != nil {
				return err
			}
			for _, t := range res.GetTeams() {
				names["team:"+t.GetId()] = t.GetName()
			}
		}
		if len(ol.Members) > 0 {
			res, err := c.members()
			if err != nil {
				return err
			}
			for _, m := range res.GetMembers() {
				names["user:"+m.GetId()] = m.GetName()
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
	return c.emit(raw, []string{"scope", "vcpu", "ram", "disk", "vms", "snapshots", "images"}, rows)
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
		scope = "team:" + t.GetId()
	case kind == "user" && key != "":
		m, err := c.findMember(key)
		if err != nil {
			return err
		}
		scope = "user:" + m.GetId()
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

func (c *cli) sessions() (*iamv1.ListSessionsResponse, error) {
	res := &iamv1.ListSessionsResponse{}
	return res, c.rpc("ListSessions", &iamv1.ListSessionsRequest{}, res)
}

func cmdSessionList(c *cli, args []string) error {
	if _, err := c.parse(c.flags("session list"), args, 0, 0); err != nil {
		return err
	}
	res, err := c.sessions()
	if err != nil {
		return err
	}
	rows := make([][]string, len(res.GetSessions()))
	for i, s := range res.GetSessions() {
		mark := ""
		if s.GetCurrent() {
			mark = "*"
		}
		rows[i] = []string{mark, s.GetId(), s.GetKind(), s.GetIp(), s.GetUserAgent(), ago(c.now(), s.GetLastUsedAt().AsTime()), s.GetExpiresAt().AsTime().In(c.now().Location()).Format("2006-01-02 15:04")}
	}
	return c.emit(pj(res), []string{"", "id", "kind", "ip", "user agent", "last used", "expires"}, rows)
}

func cmdSessionRevoke(c *cli, args []string) error {
	pos, err := c.parse(c.flags("session revoke"), args, 1, 1)
	if err != nil {
		return err
	}
	res, err := c.sessions()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(res.GetSessions(), func(s *iamv1.Session) bool { return s.GetId() == pos[0] })
	if i < 0 {
		return notFoundf("session %q not found", pos[0])
	}
	current := res.GetSessions()[i].GetCurrent()
	if err := c.confirm(fmt.Sprintf("Revoke session %s?", pos[0])); err != nil {
		return err
	}
	if err := c.rpc("RevokeSession", &iamv1.RevokeSessionRequest{Session: pos[0]}, &iamv1.RevokeSessionResponse{}); err != nil {
		var ae *apiError
		if !(current && errors.As(err, &ae) && ae.Status == http.StatusUnauthorized) {
			return err
		}
	}
	if current {
		c.cfg.Token, c.cfg.ExpiresAt = "", time.Time{}
		if err := saveConfig(c.cfgDir, c.cfg); err != nil {
			return err
		}
	}
	return c.done(nil, "Session %s revoked.", pos[0])
}
