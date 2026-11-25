package accounts

import (
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/policy"
	"github.com/asterism-dev/observatory/internal/store"
)

type Diff struct {
	User, Org      uuid.UUID
	Object, Action string
	Reason         string
}

// Report counts old/new decisions; Widenings is keyed by the new action.
type Report struct {
	Regressions []Diff
	Widenings   map[string]int
	// Collapsed lists custom-role rules on scopes that merge into one account, which no new rule can reproduce exactly.
	Collapsed     []string
	Same, Skipped int
}

// Compare evaluates every catalog permission of every org member against every existing resource in both models.
func Compare(p authz.Policy, s store.AccountSnapshot) (Report, error) {
	e, err := authz.New(p)
	if err != nil {
		return Report{}, err
	}
	rights := Derive(p, s)
	rep := Report{Widenings: map[string]int{}}
	for _, role := range p.Roles {
		for _, r := range role.Rules {
			prefix, _, _ := strings.Cut(strings.TrimPrefix(r.Object, "org/"), "/")
			if m, ok := objectMap["org/"+prefix]; ok && m.scope == "user" {
				rep.Collapsed = append(rep.Collapsed, fmt.Sprintf("%s: %s %s on %s", role.Name, r.Effect, r.Action, r.Object))
			}
		}
	}

	orgOfAccount := map[uuid.UUID]uuid.UUID{}
	teamsOf := map[uuid.UUID][]uuid.UUID{}
	for _, a := range s.Accounts {
		orgOfAccount[a.ID] = a.OrgID
		if a.TeamID != nil {
			teamsOf[a.OrgID] = append(teamsOf[a.OrgID], *a.TeamID)
		}
	}
	for _, t := range p.Teams {
		if !containsID(teamsOf[t.OrgID], t.ID) {
			teamsOf[t.OrgID] = append(teamsOf[t.OrgID], t.ID)
		}
	}
	membersOf := map[uuid.UUID][]uuid.UUID{}
	for _, m := range p.Memberships {
		membersOf[m.OrgID] = append(membersOf[m.OrgID], m.UserID)
	}
	objects := func(org uuid.UUID, pattern string) []string {
		prefix, wild := strings.CutSuffix(pattern, "/*")
		if !wild {
			return []string{pattern}
		}
		m, ok := objectMap[prefix]
		if !ok {
			return nil
		}
		var out []string
		switch m.scope {
		case "team":
			for _, t := range teamsOf[org] {
				out = append(out, prefix+"/"+t.String())
			}
		case "user":
			for _, u := range membersOf[org] {
				out = append(out, prefix+"/"+u.String())
			}
		default:
			for key, acc := range s.Resources {
				if typ, id, _ := strings.Cut(key, "/"); typ == m.typ && orgOfAccount[acc] == org {
					out = append(out, prefix+"/"+id)
				}
			}
		}
		return out
	}

	for _, m := range p.Memberships {
		for _, perm := range authz.Catalog {
			objs := objects(m.OrgID, perm.Object)
			if objs == nil {
				rep.Skipped++
				continue
			}
			for _, obj := range objs {
				old := e.Allowed(authz.UserSubject(m.UserID), m.OrgID, obj, perm.Action)
				q, ok := Translate(s, m.OrgID, obj, perm.Action)
				if !ok {
					rep.Skipped++
					continue
				}
				d := policy.Decide(rights[m.UserID], q)
				switch {
				case old == d.Allowed:
					rep.Same++
				case old:
					rep.Regressions = append(rep.Regressions, Diff{User: m.UserID, Org: m.OrgID, Object: obj, Action: perm.Action, Reason: d.Reason})
				default:
					rep.Widenings[q.Action]++
				}
			}
		}
	}
	return rep, nil
}
