// Package catalog converts, validates and registers service action catalogs.
package catalog

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/permission"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const (
	MaxActions      = 500
	MaxRoleDefaults = 500
	MaxTemplates    = 200
)

var (
	actionObjectRE   = regexp.MustCompile(`^org/[a-z][a-z-]*(/\*)?$`)
	defaultObjectRE  = regexp.MustCompile(`^org/[a-z][a-z-]*(\*|/\*)?$`)
	templateObjectRE = regexp.MustCompile(`^org/[a-z][a-z-]*(/\{(user|team|id)\})?$`)
	// Never `*` alone or a leading wildcard, so a pattern stays inside one service.
	actionWildcardRE = regexp.MustCompile(`^[a-z][a-z0-9]*\.([a-z][a-zA-Z0-9]*\.)?\*$`)
)

// FromAnnotations lists the org actions of services' permission annotations; {field} placeholders become `*`, and a name+object is a write if any method writes.
func FromAnnotations(services ...protoreflect.ServiceDescriptor) ([]authz.Action, error) {
	var out []authz.Action
	for _, sd := range services {
		methods := sd.Methods()
		for i := range methods.Len() {
			md := methods.Get(i)
			perm, ok := permission.Of(md)
			// Membership-only methods and user/platform objects are decided by no org rule.
			if !ok || perm.AnyMember || !strings.HasPrefix(perm.Object, "org/") {
				continue
			}
			if !permission.ValidName(perm.Name) {
				return nil, fmt.Errorf("%s: invalid action %q", md.FullName(), perm.Name)
			}
			obj, err := wildcardFields(perm.Object)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", md.FullName(), err)
			}
			if j := slices.IndexFunc(out, func(a authz.Action) bool { return a.Name == perm.Name && a.Object == obj }); j >= 0 {
				out[j].Write = out[j].Write || perm.Write
				continue
			}
			out = append(out, authz.Action{Name: perm.Name, Object: obj, Write: perm.Write})
		}
	}
	return out, nil
}

func wildcardFields(obj string) (string, error) {
	var b strings.Builder
	for {
		before, after, found := strings.Cut(obj, "{")
		if strings.Contains(before, "}") {
			return "", fmt.Errorf("unbalanced object %q", obj)
		}
		b.WriteString(before)
		if !found {
			return b.String(), nil
		}
		_, tail, ok := strings.Cut(after, "}")
		if !ok {
			return "", fmt.Errorf("unbalanced object %q", obj)
		}
		b.WriteString("*")
		obj = tail
	}
}

var relations = map[authz.Relation]iamv1.Relation{
	authz.RelMember:         iamv1.Relation_RELATION_MEMBER,
	authz.RelTeam:           iamv1.Relation_RELATION_TEAM,
	authz.RelTeamAdmin:      iamv1.Relation_RELATION_TEAM_ADMIN,
	authz.RelOwnerUser:      iamv1.Relation_RELATION_OWNER_USER,
	authz.RelOwnerTeam:      iamv1.Relation_RELATION_OWNER_TEAM,
	authz.RelOwnerTeamAdmin: iamv1.Relation_RELATION_OWNER_TEAM_ADMIN,
}

func ToProto(c authz.Catalog) *iamv1.Catalog {
	out := &iamv1.Catalog{Service: c.Service}
	for _, a := range c.Actions {
		out.Actions = append(out.Actions, &iamv1.CatalogAction{Name: a.Name, Object: a.Object, Description: a.Description, Write: a.Write})
	}
	for _, d := range c.RoleDefaults {
		out.RoleDefaults = append(out.RoleDefaults, &iamv1.RoleDefault{Role: d.Role, Object: d.Object, Action: d.Action})
	}
	for _, t := range c.Templates {
		out.Templates = append(out.Templates, &iamv1.OwnerTemplate{Relation: relations[t.Relation], Object: t.Object, Actions: slices.Clone(t.Actions)})
	}
	return out
}

func FromProto(p *iamv1.Catalog) (authz.Catalog, error) {
	out := authz.Catalog{Service: p.GetService()}
	for _, a := range p.GetActions() {
		out.Actions = append(out.Actions, authz.Action{Name: a.GetName(), Object: a.GetObject(), Description: a.GetDescription(), Write: a.GetWrite()})
	}
	for _, d := range p.GetRoleDefaults() {
		out.RoleDefaults = append(out.RoleDefaults, authz.RoleDefault{Role: d.GetRole(), Object: d.GetObject(), Action: d.GetAction()})
	}
	for i, t := range p.GetTemplates() {
		rel := relationOf(t.GetRelation())
		if rel == "" {
			return authz.Catalog{}, fmt.Errorf("template %d: invalid relation %v", i, t.GetRelation())
		}
		out.Templates = append(out.Templates, authz.OwnerTemplate{Relation: rel, Object: t.GetObject(), Actions: slices.Clone(t.GetActions())})
	}
	return out, nil
}

func relationOf(r iamv1.Relation) authz.Relation {
	for rel, pr := range relations {
		if pr == r {
			return rel
		}
	}
	return ""
}

// Normalize returns c with actions, defaults, templates and each template's actions sorted, and empty lists nil.
func Normalize(c authz.Catalog) authz.Catalog {
	out := authz.Catalog{
		Service:      c.Service,
		Actions:      append([]authz.Action(nil), c.Actions...),
		RoleDefaults: append([]authz.RoleDefault(nil), c.RoleDefaults...),
	}
	for _, t := range c.Templates {
		t.Actions = append([]string(nil), t.Actions...)
		slices.Sort(t.Actions)
		out.Templates = append(out.Templates, t)
	}
	slices.SortFunc(out.Actions, func(a, b authz.Action) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Object, b.Object))
	})
	slices.SortFunc(out.RoleDefaults, func(a, b authz.RoleDefault) int {
		return cmp.Or(cmp.Compare(a.Role, b.Role), cmp.Compare(a.Object, b.Object), cmp.Compare(a.Action, b.Action))
	})
	slices.SortFunc(out.Templates, func(a, b authz.OwnerTemplate) int {
		return cmp.Or(cmp.Compare(a.Relation, b.Relation), cmp.Compare(a.Object, b.Object), slices.Compare(a.Actions, b.Actions))
	})
	return out
}

// Digest is the hex SHA-256 of the normalized catalog's deterministic proto encoding.
func Digest(c authz.Catalog) string {
	// Deterministic marshalling of a plain message cannot fail.
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(ToProto(Normalize(c)))
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Validate checks c's limits and patterns, and that every action it names starts with one of allowedPrefixes; the error names the first offending entry.
// allowedPrefixes must come from the caller's service token, never from c.Service.
func Validate(c authz.Catalog, allowedPrefixes []string) error {
	switch {
	case len(c.Actions) > MaxActions:
		return fmt.Errorf("%d actions, at most %d", len(c.Actions), MaxActions)
	case len(c.RoleDefaults) > MaxRoleDefaults:
		return fmt.Errorf("%d role defaults, at most %d", len(c.RoleDefaults), MaxRoleDefaults)
	case len(c.Templates) > MaxTemplates:
		return fmt.Errorf("%d templates, at most %d", len(c.Templates), MaxTemplates)
	}
	own := func(action string) bool {
		svc, _, _ := strings.Cut(action, ".")
		return slices.Contains(allowedPrefixes, svc)
	}
	seen := map[authz.Permission]bool{}
	declared := map[string]bool{}
	for _, a := range c.Actions {
		if !permission.ValidName(a.Name) || !own(a.Name) {
			return fmt.Errorf("action %s on %s: name outside %v", a.Name, a.Object, allowedPrefixes)
		}
		if !actionObjectRE.MatchString(a.Object) {
			return fmt.Errorf("action %s on %s: invalid object", a.Name, a.Object)
		}
		key := authz.Permission{Action: a.Name, Object: a.Object}
		if seen[key] {
			return fmt.Errorf("action %s on %s: duplicate", a.Name, a.Object)
		}
		seen[key] = true
		declared[a.Name] = true
	}
	// A pattern or declared action of the caller's own services; never another service's.
	grantable := func(action string) bool {
		if actionWildcardRE.MatchString(action) {
			return own(action)
		}
		return declared[action]
	}
	for _, d := range c.RoleDefaults {
		if d.Role != "member" && d.Role != "admin" && d.Role != "owner" {
			return fmt.Errorf("role default %s %s on %s: unknown role", d.Role, d.Action, d.Object)
		}
		if !defaultObjectRE.MatchString(d.Object) {
			return fmt.Errorf("role default %s %s on %s: invalid object", d.Role, d.Action, d.Object)
		}
		if !grantable(d.Action) {
			return fmt.Errorf("role default %s %s on %s: action not declared or outside %v", d.Role, d.Action, d.Object, allowedPrefixes)
		}
	}
	for _, t := range c.Templates {
		if err := validTemplateObject(t.Relation, t.Object); err != nil {
			return fmt.Errorf("template %s on %s: %w", t.Relation, t.Object, err)
		}
		for _, act := range t.Actions {
			if !grantable(act) {
				return fmt.Errorf("template %s on %s: action %s not declared or outside %v", t.Relation, t.Object, act, allowedPrefixes)
			}
		}
	}
	return nil
}

func validTemplateObject(rel authz.Relation, obj string) error {
	m := templateObjectRE.FindStringSubmatch(obj)
	if m == nil {
		return errors.New("invalid object")
	}
	placeholder := m[2]
	switch rel {
	case authz.RelMember:
		if placeholder == "" || placeholder == "user" {
			return nil
		}
	case authz.RelTeam, authz.RelTeamAdmin:
		if placeholder == "" || placeholder == "team" {
			return nil
		}
	case authz.RelOwnerUser, authz.RelOwnerTeam, authz.RelOwnerTeamAdmin:
		if placeholder == "id" {
			return nil
		}
	default:
		return errors.New("unknown relation")
	}
	return fmt.Errorf("placeholder {%s} not valid here", placeholder)
}

// ponytail: hub registers the actions of services not yet extracted; drop each prefix when its service registers its own catalog.
var hubPrefixes = []string{"compute", "network", "secrets", "artifacts", "build", "telemetry", "quota"}

func AllowedPrefixes(caller string) []string {
	if caller == "hub" {
		return append([]string{caller}, hubPrefixes...)
	}
	return []string{caller}
}

var retryBase, retryMax = time.Second, 30 * time.Second

// Register sends c to iam until it answers, retrying Unavailable (which covers transport errors) with backoff until ctx ends.
func Register(ctx context.Context, client iamv1connect.CatalogInternalServiceClient, c authz.Catalog) (string, error) {
	req := &iamv1.RegisterCatalogRequest{Catalog: ToProto(c)}
	wait := retryBase
	for {
		resp, err := client.RegisterCatalog(ctx, connect.NewRequest(req))
		if err == nil {
			return resp.Msg.GetDigest(), nil
		}
		if ctx.Err() != nil {
			return "", fmt.Errorf("register catalog: %w (last error: %w)", ctx.Err(), err)
		}
		if connect.CodeOf(err) != connect.CodeUnavailable {
			return "", fmt.Errorf("register catalog: %w", err)
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("register catalog: %w (last error: %w)", ctx.Err(), err)
		case <-time.After(wait):
		}
		wait = min(2*wait, retryMax)
	}
}
