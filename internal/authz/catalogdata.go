package authz

type Action struct {
	Name, Object, Description string
	Write                     bool
}

// RoleDefault grants Action on Object to Role (member, admin or owner) and every role above it.
type RoleDefault struct {
	Role, Object, Action string
}

type Relation string

const (
	RelMember         Relation = "member"
	RelTeam           Relation = "team"
	RelTeamAdmin      Relation = "team_admin"
	RelOwnerUser      Relation = "owner_user"
	RelOwnerTeam      Relation = "owner_team"
	RelOwnerTeamAdmin Relation = "owner_team_admin"
)

// OwnerTemplate grants Actions on Object, whose {user}, {team} or {id} the enforcer fills per subject.
type OwnerTemplate struct {
	Relation Relation
	Object   string
	Actions  []string
}

type Catalog struct {
	Service      string
	Actions      []Action
	RoleDefaults []RoleDefault
	Templates    []OwnerTemplate
}
