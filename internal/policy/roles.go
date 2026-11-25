package policy

// Services are the services whose resources an account member works with; iam is administration.
var Services = []string{"compute", "secrets", "integrations", "images", "telemetry"}

var accountResources = []string{"obs:*:${account}:*"}

// BuiltinRole returns a fresh copy of a built-in account role's rules, or nil for an unknown key.
func BuiltinRole(key string) []Rule {
	switch key {
	case "viewer":
		return []Rule{{Effect: Allow, Actions: []string{"*:*:read", "*:*:list"}, Resources: accountResources}}
	case "member":
		actions := make([]string, len(Services))
		for i, s := range Services {
			actions[i] = s + ":*"
		}
		return []Rule{{Effect: Allow, Actions: actions, Resources: accountResources}}
	case "admin":
		return append(BuiltinRole("member"), Rule{Effect: Allow, Actions: []string{"iam:*"}, Resources: []string{"obs:iam:${account}:*"}})
	}
	return nil
}
