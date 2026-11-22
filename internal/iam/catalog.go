package iam

import (
	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/catalog"
)

func Catalog() (authz.Catalog, error) {
	actions, err := catalog.FromAnnotations(iamv1.File_observatory_iam_v1_iam_proto.Services().ByName("IAMService"))
	if err != nil {
		return authz.Catalog{}, err
	}
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
	}, nil
}
