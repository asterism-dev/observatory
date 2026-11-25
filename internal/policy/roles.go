package policy

// Services are the services whose resources an account member works with; iam is administration.
var Services = []string{"compute", "secrets", "integrations", "images", "telemetry"}

// Resources in other accounts are reachable only where their owner shares them, so the identity side may name any account.
func anyResource() []string { return []string{"obs:*:*:*"} }

// BuiltinRole returns a fresh copy of a built-in account role's rules, or nil for an unknown key.
func BuiltinRole(key string) []Rule {
	switch key {
	case "viewer":
		return []Rule{{Effect: Allow, Actions: []string{"*:*:read", "*:*:list"}, Resources: anyResource()}}
	case "member":
		// Node management and telemetry tokens stay with admins (user decision); members place VMs on nodes.
		return []Rule{{Effect: Allow, Actions: []string{
			"compute:vm:*", "compute:disk:*", "compute:network:*", "compute:node:read", "compute:node:list", "compute:node:use",
			"secrets:*", "integrations:*", "images:*", "telemetry:dashboard:*", "telemetry:logs:*",
		}, Resources: anyResource()}}
	case "admin":
		return append(BuiltinRole("member"),
			Rule{Effect: Allow, Actions: []string{"compute:node:*", "telemetry:token:*"}, Resources: anyResource()},
			Rule{Effect: Allow, Actions: []string{"iam:*"}, Resources: []string{"obs:iam:${account}:*"}})
	}
	return nil
}
