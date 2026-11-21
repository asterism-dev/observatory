package authz

import (
	"reflect"
	"slices"
	"testing"

	"github.com/asterism-dev/observatory/internal/platform/permission"
	"github.com/casbin/casbin/v3/util"
)

// renamed is the binding 0.x → 1.0 permission mapping; its keys are every 0.x catalog action.
var renamed = map[string]string{
	"org:update":          "iam.orgs.update",
	"node:read":           "compute.nodes.get",
	"member:read":         "iam.members.list",
	"node:create":         "compute.nodes.create",
	"member:invite":       "iam.invites.create",
	"node:update":         "compute.nodes.update",
	"member:remove":       "iam.members.delete",
	"node:delete":         "compute.nodes.delete",
	"member:set-role":     "iam.members.setRole",
	"node:use":            "compute.nodes.use",
	"team:read":           "iam.teams.list",
	"node:metrics":        "compute.nodes.getMetrics",
	"team:create":         "iam.teams.create",
	"vm:create":           "compute.instances.create",
	"team:update":         "iam.teams.update",
	"vm:read":             "compute.instances.get",
	"team:delete":         "iam.teams.delete",
	"vm:power":            "compute.instances.setPower",
	"team:manage-members": "iam.teams.manageMembers",
	"vm:delete":           "compute.instances.delete",
	"team:leave":          "iam.teams.leave",
	"vm:network":          "compute.instances.setNetworks",
	"role:read":           "iam.roles.list",
	"vm:disks":            "compute.instances.setDisks",
	"role:write":          "iam.roles.manage",
	"vm:connect":          "compute.instances.connect",
	"role:bind":           "iam.roleBindings.manage",
	"vm:snapshot":         "compute.instances.snapshot",
	"limit:read":          "quota.limits.get",
	"vm:restore":          "compute.instances.restore",
	"limit:set":           "quota.limits.update",
	"vm:fork":             "compute.instances.fork",
	"audit:read":          "audit.events.list",
	"vm:metrics":          "compute.instances.getMetrics",
	"logs:read":           "telemetry.logEntries.list",
	"disk:create":         "compute.disks.create",
	"telemetry:token":     "telemetry.tokens.create",
	"disk:read":           "compute.disks.get",
	"dashboard:read":      "telemetry.dashboards.get",
	"disk:update":         "compute.disks.update",
	"dashboard:edit":      "telemetry.dashboards.update",
	"disk:delete":         "compute.disks.delete",
	"dashboard:create":    "telemetry.dashboards.create",
	"disk:use":            "compute.disks.use",
	"device:create":       "network.devices.create",
	"env:list":            "secrets.variables.list",
	"device:list":         "network.devices.list",
	"env:set":             "secrets.variables.set",
	"device:delete":       "network.devices.delete",
	"env:delete":          "secrets.variables.delete",
	"network:read":        "network.networks.get",
	"env:use":             "secrets.variables.use",
	"network:create":      "network.networks.create",
	"integration:list":    "secrets.integrations.list",
	"network:use":         "network.networks.use",
	"integration:set":     "secrets.integrations.set",
	"network:delete":      "network.networks.delete",
	"integration:delete":  "secrets.integrations.delete",
	"image:list":          "artifacts.images.list",
	"integration:use":     "secrets.integrations.use",
	"image:use":           "artifacts.images.use",
	"image:edit":          "artifacts.images.update",
	"image:default":       "artifacts.images.setDefault",
	"image:build":         "build.builds.create",
}

// addedSince are the actions added after the rename, granted to admin and owner on ObjAudit.
var addedSince = []string{"audit.settings.get", "audit.settings.update"}

// The 0.x built-in roles, captured verbatim before the rename.
var (
	oldMember = []Rule{
		{Allow, ObjMembers, "member:read"},
		{Allow, ObjTeams + "*", "team:read"},
		{Allow, ObjTeams + "*", "team:leave"},
		{Allow, ObjRoles, "role:read"},
		{Allow, ObjLimits, "limit:read"},
		{Allow, ObjEnv, "env:list"},
		{Allow, ObjEnv, "env:use"},
		{Allow, ObjIntegrations, "integration:list"},
		{Allow, ObjIntegrations, "integration:use"},
		{Allow, ObjImages, "image:list"},
		{Allow, ObjImages, "image:use"},
		{Allow, ObjDashboards, "dashboard:read"},
		{Allow, ObjNodes, "node:read"},
		{Allow, ObjNodes + "/*", "node:read"},
		{Allow, ObjNodes + "/*", "node:use"},
		{Allow, ObjVMs, "vm:create"},
		{Allow, ObjNetworks, "network:read"},
		{Allow, ObjNetworks, "network:create"},
		{Allow, ObjNetworks + "/*", "network:use"},
		{Allow, ObjDisks, "disk:create"},
	}
	oldAdminExtra = []Rule{
		{Allow, ObjSettings, "org:update"},
		{Allow, ObjSettings, "telemetry:token"},
		{Allow, ObjMembers, "member:invite"},
		{Allow, ObjMembers, "member:remove"},
		{Allow, ObjTeams + "*", "team:*"},
		{Allow, ObjRoles, "role:write"},
		{Allow, ObjRoles, "role:bind"},
		{Allow, ObjLimits, "limit:set"},
		{Allow, ObjAudit, "audit:read"},
		{Allow, ObjLogs, "logs:read"},
		{Allow, ObjEnv, "env:*"},
		{Allow, ObjTeamEnv + "/*", "env:*"},
		{Allow, ObjIntegrations, "integration:*"},
		{Allow, ObjTeamIntegrations + "/*", "integration:*"},
		{Allow, ObjImages, "image:*"},
		{Allow, ObjTeamImages + "/*", "image:*"},
		{Allow, ObjDashboards, "dashboard:*"},
		{Allow, ObjTeamDashboards + "/*", "dashboard:*"},
		{Allow, ObjNodes, "node:*"},
		{Allow, ObjNodes + "/*", "node:*"},
		{Allow, ObjTeamVMs + "/*", "vm:create"},
		{Allow, ObjVMs + "/*", "vm:read"},
		{Allow, ObjVMs + "/*", "vm:power"},
		{Allow, ObjVMs + "/*", "vm:delete"},
		{Allow, ObjVMs + "/*", "vm:network"},
		{Allow, ObjVMs + "/*", "vm:snapshot"},
		{Allow, ObjVMs + "/*", "vm:restore"},
		{Allow, ObjVMs + "/*", "vm:metrics"},
		{Allow, ObjUserDevices + "/*", "device:list"},
		{Allow, ObjUserDevices + "/*", "device:delete"},
		{Allow, ObjNetworks + "/*", "network:delete"},
		{Allow, ObjTeamDisks + "/*", "disk:create"},
		{Allow, ObjDisks + "/*", "disk:read"},
		{Allow, ObjDisks + "/*", "disk:update"},
		{Allow, ObjDisks + "/*", "disk:delete"},
		{Allow, ObjVMs + "/*", "vm:disks"},
	}
	oldOwnerExtra = []Rule{{Allow, ObjMembers, "member:set-role"}}
)

func TestCatalogNamesValid(t *testing.T) {
	for _, p := range Catalog {
		if !permission.ValidName(p.Action) {
			t.Errorf("catalog action %q@%s is not service.resource.verb", p.Action, p.Object)
		}
	}
	var got, want []string
	for _, p := range Catalog {
		if !slices.Contains(got, p.Action) {
			got = append(got, p.Action)
		}
	}
	for _, n := range renamed {
		want = append(want, n)
	}
	want = append(want, addedSince...)
	slices.Sort(got)
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("catalog actions = %v, want the mapping's targets %v", got, want)
	}
}

// grants flattens rules into "effect object action" entries, one per catalog action a pattern matches.
func grants(rules []Rule, expand func(string) []string) []string {
	var out []string
	for _, r := range rules {
		for _, a := range expand(r.Action) {
			if g := string(r.Effect) + " " + r.Object + " " + a; !slices.Contains(out, g) {
				out = append(out, g)
			}
		}
	}
	slices.Sort(out)
	return out
}

func TestBuiltinRolesUnchangedByRename(t *testing.T) {
	expandOld := func(pattern string) []string {
		var out []string
		for old, n := range renamed {
			if util.KeyMatch(old, pattern) {
				out = append(out, n)
			}
		}
		return out
	}
	admin := slices.Concat(oldMember, oldAdminExtra)
	for key, old := range map[string][]Rule{"member": oldMember, "admin": admin, "owner": slices.Concat(admin, oldOwnerExtra)} {
		got, want := grants(BuiltinRules(key), ExpandActions), grants(old, expandOld)
		if key != "member" {
			for _, a := range addedSince {
				want = append(want, "allow "+ObjAudit+" "+a)
			}
			slices.Sort(want)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s grants\n got %v\nwant %v", key, got, want)
		}
	}
}

func TestWildcardPatterns(t *testing.T) {
	instances := []string{
		"compute.instances.create", "compute.instances.get", "compute.instances.setPower", "compute.instances.delete",
		"compute.instances.connect", "compute.instances.setNetworks", "compute.instances.setDisks", "compute.instances.snapshot",
		"compute.instances.restore", "compute.instances.fork", "compute.instances.getMetrics",
	}
	if got := ExpandActions("compute.instances.*"); !reflect.DeepEqual(got, instances) {
		t.Errorf("compute.instances.* = %v, want %v", got, instances)
	}
	var compute []string
	for _, n := range renamed {
		if len(n) > 8 && n[:8] == "compute." {
			compute = append(compute, n)
		}
	}
	got := ExpandActions("compute.*")
	slices.Sort(got)
	slices.Sort(compute)
	if !reflect.DeepEqual(got, compute) {
		t.Errorf("compute.* = %v, want %v", got, compute)
	}
	if got := ExpandActions("*"); len(got) != len(renamed)+len(addedSince) {
		t.Errorf("* expands to %d actions, want %d", len(got), len(renamed)+len(addedSince))
	}
	// A wildcard must cover whole segments, or compute* would also grant a future computex service.
	for _, p := range []string{"compute*", "compute.inst*", "compute.instances.set*", "*.instances.get", "compute.*.get", "compute.instances.get*"} {
		if got := ExpandActions(p); len(got) != 0 {
			t.Errorf("ExpandActions(%q) = %v, want none", p, got)
		}
	}
}
