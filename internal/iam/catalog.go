package iam

import (
	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/catalog"
)

// ponytail: hub registers the actions of services not yet extracted; drop each prefix when its service registers its own catalog.
var hubPrefixes = []string{"compute", "network", "secrets", "artifacts", "build", "telemetry", "quota"}

// AllowedPrefixes are the action prefixes caller may register: its own name, plus the transitional hub list.
func AllowedPrefixes(caller string) []string {
	if caller == "hub" {
		return append([]string{caller}, hubPrefixes...)
	}
	return []string{caller}
}

func Catalog() authz.Catalog {
	// TestRegisteredCatalogsReproduce0x proves these annotations convert without error.
	actions, _ := catalog.FromAnnotations(iamv1.File_observatory_iam_v1_iam_proto.Services().ByName("IAMService"))
	return authz.Catalog{
		Service: "iam",
		Actions: actions,
		RoleDefaults: []authz.RoleDefault{
			{Role: "member", Object: "org/members", Action: "iam.members.list"},
			{Role: "member", Object: "org/teams*", Action: "iam.teams.list"},
			{Role: "member", Object: "org/teams*", Action: "iam.teams.leave"},
			{Role: "member", Object: "org/roles", Action: "iam.roles.list"},
			{Role: "admin", Object: "org/settings", Action: "iam.orgs.update"},
			{Role: "admin", Object: "org/members", Action: "iam.invites.create"},
			{Role: "admin", Object: "org/members", Action: "iam.members.delete"},
			{Role: "admin", Object: "org/teams*", Action: "iam.teams.*"},
			{Role: "admin", Object: "org/roles", Action: "iam.roles.manage"},
			{Role: "admin", Object: "org/roles", Action: "iam.roleBindings.manage"},
			{Role: "owner", Object: "org/members", Action: "iam.members.setRole"},
		},
		Templates: []authz.OwnerTemplate{
			{Relation: authz.RelTeamAdmin, Object: "org/teams/{team}", Actions: []string{"iam.teams.manageMembers"}},
		},
	}
}
